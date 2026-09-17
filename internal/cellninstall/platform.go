package cellninstall

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/internal/cellnparent"
	"github.com/sympozium-ai/sympozium/internal/cellnplatform"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SessionDefaults is the budget a new conversation asks for: a working
// session that stays well inside the scope's ceilings — a four-hour parent
// with 64 turns and the starter profile's per-turn allowance (3 requests,
// 1536 output tokens) for each — capped by the ceilings themselves.
func SessionDefaults(ceilings api.EnduringRunSpec) *api.EnduringRunSpec {
	return &api.EnduringRunSpec{LeaseSeconds: min(14400, ceilings.LeaseSeconds), MaxTurns: min(64, ceilings.MaxTurns), MaxModelRequests: min(192, ceilings.MaxModelRequests), MaxOutputTokens: min(98304, ceilings.MaxOutputTokens)}
}

// ScopeLabel opts a namespace into a scope in strict ("labeled") mode; the
// default mode admits every namespace except the system exclusions.
const ScopeLabel = cellnplatform.ScopeLabel

const packageAnnotation = "celln.sympozium.ai/package"

// PlatformOptions publishes one reviewed starter configuration as the
// cluster-scoped platform catalogue and wires one tenant namespace to it.
type PlatformOptions struct {
	Namespace, ConfigurationDir, OutputDir string
	Scope, ClusterID, PackageHash          string
	Principal                              string
	ControllerNamespace                    string
	// Authorise is "all" (default: every namespace except the system
	// exclusions and namespaces labeled excluded) or "labeled" (only
	// namespaces carrying ScopeLabel).
	Authorise string
	// Replacing is the publication this installation replaces, when the
	// operator approved moving the scope to a new package (or scope). Its
	// catalogue objects are replaced or removed and every namespace's
	// platform wrappers are rebound; zero means nothing is replaced.
	Replacing FleetPublication
}

// PlatformCatalogueNames are the cluster-scoped objects one scope publishes:
// its policy and its tool names. Profiles are named per backend by
// PlatformProfileName.
func PlatformCatalogueNames(scope string) (profile, policy string, tool func(string) string) {
	return PlatformProfileName(scope, cellnplatform.DefaultBackend), "celln-fleet-" + scope, func(name string) string { return "celln-" + scope + "-" + name }
}

// PlatformProfileName is the cluster-scoped runtime profile of one backend.
func PlatformProfileName(scope, backend string) string {
	if backend == cellnplatform.DefaultBackend {
		return "celln-native-" + scope
	}
	return "celln-native-" + scope + "-" + backend
}

// sameJSON compares two JSON documents by value, not by formatting.
func sameJSON(a, b []byte) bool {
	var left, right any
	return json.Unmarshal(a, &left) == nil && json.Unmarshal(b, &right) == nil && reflect.DeepEqual(left, right)
}

// backendConfiguration is one backend's reviewed starter configuration as a
// fleet node published it.
type backendConfiguration struct {
	name       string
	cat        catalogue
	configured receipt
	native     cellnparent.NativeProvisionConfig
	protocol   string
	origin     string
	insecure   bool
	harness    struct {
		Contract      string `json:"contract"`
		System        string `json:"system"`
		Model         string `json:"model"`
		URL           string `json:"url"`
		AllowInsecure bool   `json:"allow_insecure"`
	}
}

func readBackendConfiguration(dir, backend, packageHash, principal string) (backendConfiguration, error) {
	b := backendConfiguration{name: backend}
	hashes := map[string]string{}
	for file, target := range map[string]any{"catalogue.json": &b.cat, "configured.json": &b.configured, "native-template.json": &b.native} {
		hash, err := read(filepath.Join(dir, backend, file), target)
		if err != nil {
			return b, fmt.Errorf("backend %s: %w", backend, err)
		}
		hashes[file] = hash
	}
	c := b.configured
	if c.PackageHash != packageHash || c.CatalogueHash != hashes["catalogue.json"] || c.NativeTemplateHash != hashes["native-template.json"] {
		return b, fmt.Errorf("backend %s: configuration differs from reviewed package/receipt", backend)
	}
	if c.APIVersion != "celln.native-starter-configured/v1" || c.ExecutionAuthorized || c.Readiness != "not_established" || b.native.ModelProfile != c.ModelProfile || c.Principal != principal || len(b.cat.Tools) < 3 || len(b.cat.Tools) > 24 || b.cat.Worker.ContractVersion != "celln.json-tools/v1" {
		return b, fmt.Errorf("backend %s: operator starter configuration mismatch", backend)
	}
	if json.Unmarshal(b.native.Template, &b.harness) != nil || b.harness.Contract != "celln.json-tools/v1" || b.harness.System != b.cat.SystemPrompt || b.harness.Model != c.Model.Model {
		return b, fmt.Errorf("backend %s: native template does not match the starter catalogue", backend)
	}
	b.insecure = b.harness.AllowInsecure || c.Model.AllowInsecure
	origin, err := api.ModelEndpointOriginInsecure(b.harness.URL, b.insecure)
	if err != nil {
		return b, fmt.Errorf("backend %s: %w", backend, err)
	}
	b.origin = origin
	b.protocol = c.Model.Protocol
	if b.protocol == "" {
		b.protocol = "openai-chat"
	}
	return b, nil
}

// InstallPlatform publishes the fleet's starter configuration as one
// CellnRuntimeProfile per backend, the scope's ClusterCellnTools and one
// CellnExecutionPolicy carrying every backend's route, then creates the
// target namespace's wrapper objects for every backend. It writes a
// platform-mode registrations.json and a sample run. Existing catalogue
// objects are accepted only when they carry the same package; nothing is
// replaced and no run is submitted.
func InstallPlatform(ctx context.Context, store client.Client, o PlatformOptions) error {
	if store == nil || len(validation.IsDNS1123Label(o.Namespace)) != 0 || len(validation.IsDNS1123Label(o.Scope)) != 0 || o.ClusterID == "" || o.ControllerNamespace == "" || o.Principal == "" || !strings.HasPrefix(o.PackageHash, "blake3:") || len(o.PackageHash) != 71 {
		return fmt.Errorf("platform installation requires namespace, DNS-label scope, cluster identity, principal and package hash")
	}
	for _, path := range []string{o.ConfigurationDir, o.OutputDir} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return fmt.Errorf("clean absolute installation paths required")
		}
	}
	if ready, err := ControllerRolledOut(ctx, store, o.ControllerNamespace); err != nil {
		return err
	} else if !ready {
		return fmt.Errorf("general controller must finish its rollout before platform installation")
	}
	names, err := ConfigurationBackends(o.ConfigurationDir)
	if err != nil {
		return err
	}
	backends := make([]backendConfiguration, 0, len(names))
	for _, name := range names {
		b, err := readBackendConfiguration(o.ConfigurationDir, name, o.PackageHash, o.Principal)
		if err != nil {
			return err
		}
		backends = append(backends, b)
	}
	first := backends[0]
	var worker struct {
		Capabilities struct {
			TimeoutMs int64 `json:"timeoutMs"`
		} `json:"capabilities"`
	}
	if json.Unmarshal(first.native.Worker, &worker) != nil || worker.Capabilities.TimeoutMs < 1000 {
		return fmt.Errorf("native worker request lacks a bounded lifetime")
	}
	// One scope, one package: every backend must be the same reviewed
	// material with only its model route and credential differing.
	for _, b := range backends[1:] {
		if !reflect.DeepEqual(b.cat.Tools, first.cat.Tools) || !reflect.DeepEqual(b.cat.Worker, first.cat.Worker) || b.cat.SystemPrompt != first.cat.SystemPrompt || b.configured.HostLimits != first.configured.HostLimits || !sameJSON(b.native.Parent, first.native.Parent) || !sameJSON(b.native.Worker, first.native.Worker) {
			return fmt.Errorf("backend %s differs from %s in reviewed package material; one scope carries one package", b.name, first.name)
		}
	}
	_, policyName, toolName := PlatformCatalogueNames(o.Scope)
	meta := func(name string, labels map[string]string) metav1.ObjectMeta {
		all := map[string]string{"app.kubernetes.io/part-of": "sympozium"}
		for k, v := range labels {
			all[k] = v
		}
		return metav1.ObjectMeta{Name: name, Labels: all, Annotations: map[string]string{packageAnnotation: first.configured.PackageHash}}
	}
	var objects []client.Object
	profiles := map[string]*api.CellnRuntimeProfile{}
	runtimeRefs := make([]api.CellnExecutionPolicyRuntime, 0, len(backends))
	routes := make([]api.CellnExecutionPolicyRoute, 0, len(backends))
	for _, b := range backends {
		credentialProfile := b.configured.Model.CredentialProfile
		if credentialProfile == "" {
			credentialProfile = CredentialProfileFor(o.Scope, b.name)
		}
		profileName := PlatformProfileName(o.Scope, b.name)
		cat, native := b.cat, b.native
		profileMeta := meta(profileName, map[string]string{cellnplatform.BackendLabel: b.name})
		profileMeta.Annotations[cellnplatform.ProtocolAnnotation] = b.protocol
		profile := &api.CellnRuntimeProfile{ObjectMeta: profileMeta, Spec: api.CellnRuntimeProfileSpec{
			Revision: cat.Worker.Revision, ContractVersion: cat.Worker.ContractVersion, Executable: cat.Worker.Executable, Closure: cat.Worker.Closure, Mote: cat.Worker.Mote, PublisherKey: cat.Worker.PublisherKey, EntryPoint: cat.Worker.EntryPoint, Platform: cat.Worker.Platform, Lane: cat.Worker.Lane,
			Lifecycles: []string{"disposable-one-shot", "enduring"}, Limits: cat.Worker.Limits, JSON: cat.Worker.JSON.DeepCopy(),
			Native: &api.CellnNativeProvisioning{AdmissionWindowMs: int64(native.AdmissionWindowMs), Parent: apiextensionsv1.JSON{Raw: native.Parent}, Worker: apiextensionsv1.JSON{Raw: native.Worker}, Template: apiextensionsv1.JSON{Raw: native.Template}, ModelProfile: native.ModelProfile, CredentialProfile: credentialProfile, ReservedMemoryBytes: int64(native.ReservedMemoryBytes), TurnModelRequests: int64(native.TurnModelRequests), TurnOutputTokens: int64(native.TurnOutputTokens), SystemPrompt: cat.SystemPrompt},
		}}
		profiles[b.name] = profile
		objects = append(objects, profile)
		runtimeRefs = append(runtimeRefs, api.CellnExecutionPolicyRuntime{Ref: api.CellnRuntimeProfileRef{Name: profileName, Revision: cat.Worker.Revision}})
		routes = append(routes, api.CellnExecutionPolicyRoute{Provider: b.configured.Model.Provider, Protocol: b.protocol, Models: []string{b.configured.Model.Model}, EndpointOrigins: []string{b.origin}, Auth: "host-profile", AllowInsecure: b.insecure})
	}
	policyTools := make([]api.CellnExecutionPolicyTool, 0, len(first.cat.Tools))
	clusterRefs := make([]api.ClusterCellnToolRef, 0, len(first.cat.Tools))
	for _, entry := range first.cat.Tools {
		objects = append(objects, &api.ClusterCellnTool{ObjectMeta: meta(toolName(entry.Name), nil), Spec: entry.Spec})
		policyTools = append(policyTools, api.CellnExecutionPolicyTool{Ref: api.ClusterCellnToolRef{Name: toolName(entry.Name), Revision: entry.Spec.Revision}})
		clusterRefs = append(clusterRefs, api.ClusterCellnToolRef{Name: toolName(entry.Name), Revision: entry.Spec.Revision})
	}
	limits := first.configured.HostLimits
	selector, err := cellnplatform.Selector(o.Authorise, o.Scope, cellnplatform.SystemNamespaces(o.ControllerNamespace, "celln-system"))
	if err != nil {
		return err
	}
	policy := &api.CellnExecutionPolicy{ObjectMeta: meta(policyName, nil), Spec: api.CellnExecutionPolicySpec{
		NamespaceSelector: selector,
		RuntimeProfiles:   runtimeRefs,
		Tools:             policyTools,
		Lifecycles:        []string{"direct-one-shot", "harness-one-shot", "enduring"},
		Routes:            routes,
		Ceilings:          api.CellnExecutionPolicyCeilings{MaxTurns: int64(limits.MaxTurns), MaxModelRequests: int64(limits.MaxModelRequests), MaxOutputTokens: limits.MaxOutputTokens, MaxParentLeaseSeconds: int64(limits.LeaseSeconds), MaxTurnSeconds: worker.Capabilities.TimeoutMs / 1000},
	}}
	// Reserve the private output before any cluster change; a rerun that adds
	// a backend reuses the directory it made.
	if err := os.Mkdir(o.OutputDir, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	replacing := o.Replacing.Replaces(o.Scope, first.configured.PackageHash)
	for _, object := range objects {
		if err := ensurePlatformObject(ctx, store, object, first.configured.PackageHash, replacing); err != nil {
			return err
		}
	}
	// The scope's one policy grows with its backends: a rerun adds the new
	// backend's profile and route and never removes or rewrites the others.
	if err := ensurePlatformPolicy(ctx, store, policy, first.configured.PackageHash, replacing); err != nil {
		return err
	}
	if replacing {
		if err := retirePlatformCatalogue(ctx, store, o.Replacing, o.Scope, objects, policyName); err != nil {
			return err
		}
		if err := rebindTenantWrappers(ctx, store, profiles, policy); err != nil {
			return err
		}
	}
	var namespace corev1.Namespace
	if err := store.Get(ctx, types.NamespacedName{Name: o.Namespace}, &namespace); err != nil {
		return err
	}
	if o.Authorise == cellnplatform.AuthoriseLabeled && namespace.Labels[ScopeLabel] != o.Scope {
		patch := client.MergeFrom(namespace.DeepCopy())
		if namespace.Labels == nil {
			namespace.Labels = map[string]string{}
		}
		namespace.Labels[ScopeLabel] = o.Scope
		if err := store.Patch(ctx, &namespace, patch); err != nil {
			return err
		}
	}
	// The install namespace's wrappers are the same objects the API server
	// creates on demand for any other authorised namespace: one set per backend.
	for _, b := range backends {
		wrappers, err := cellnplatform.TenantWrappers(o.Namespace, profiles[b.name], policy)
		if err != nil {
			return err
		}
		for _, object := range wrappers {
			annotations := object.GetAnnotations()
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[packageAnnotation] = first.configured.PackageHash
			object.SetAnnotations(annotations)
			if err := ensurePlatformObject(ctx, store, object, first.configured.PackageHash, replacing); err != nil {
				return err
			}
		}
	}
	// Records written once are kept on a rerun (the registration and the
	// sample run bind the first installation); installed.json is rewritten
	// because it lists the backends.
	write := func(name string, value any) error {
		raw, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return err
		}
		flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
		if name == "installed.json" {
			flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		}
		f, err := os.OpenFile(filepath.Join(o.OutputDir, name), flags, 0600)
		if os.IsExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := f.Write(raw); err != nil {
			return err
		}
		return f.Sync()
	}
	journal, approvals := filepath.Join(FleetJournalRoot, "journal"), filepath.Join(FleetJournalRoot, "approvals")
	registration := cellnparent.RegistrationConfig{APIVersion: "sympozium.ai/celln-parent-registrations-v1", Journal: journal, Approvals: approvals, Platform: &cellnparent.PlatformProvisioner{ClusterID: o.ClusterID, Journal: journal, Approvals: approvals, Target: ManagedRouterURL, TokenFile: fleetControllerTokenFile}}
	if err := write("registrations.json", registration); err != nil {
		return err
	}
	// The sample run uses the default backend when the scope has one, else
	// the first backend by name.
	sample := backends[0]
	for _, b := range backends {
		if b.name == cellnplatform.DefaultBackend {
			sample = b
		}
	}
	wrapperNames := cellnplatform.WrapperNames(sample.name)
	run := &api.AgentRun{TypeMeta: metav1.TypeMeta{APIVersion: "sympozium.ai/v1alpha1", Kind: "AgentRun"}, ObjectMeta: metav1.ObjectMeta{GenerateName: "celln-starter-", Namespace: o.Namespace}, Spec: api.AgentRunSpec{
		AgentRef: wrapperNames.Agent, Backend: "celln", ExecutionLifecycle: "enduring", SystemPrompt: sample.cat.SystemPrompt, Cleanup: "delete",
		Model:          api.ModelSpec{ConnectionRef: wrapperNames.Connection, Model: sample.configured.Model.Model},
		CellnSelection: &api.CellnCatalogueSelection{RuntimeRef: wrapperNames.Runtime, ToolRefs: []api.CellnCatalogueToolRef{}, ClusterToolRefs: clusterRefs},
		Enduring:       SessionDefaults(limits),
		Task:           api.NewStringTask("Write violet to notes.txt using workspace-write with revision 0. Make exactly that one tool call, then reply done."),
	}}
	if err := write("run.json", run); err != nil {
		return err
	}
	profileNames := make([]string, 0, len(backends))
	for _, b := range backends {
		profileNames = append(profileNames, PlatformProfileName(o.Scope, b.name))
	}
	return write("installed.json", map[string]any{"namespace": o.Namespace, "scope": o.Scope, "packageHash": first.configured.PackageHash, "backends": names, "profiles": profileNames, "policy": policyName, "runSubmitted": false, "readiness": "not_established"})
}

// ClusterIdentity is the stable cluster identity every platform decision and
// scoped parent incarnation binds: the kube-system namespace UID.
func ClusterIdentity(ctx context.Context, store client.Reader) (string, error) {
	var system corev1.Namespace
	if err := store.Get(ctx, types.NamespacedName{Name: "kube-system"}, &system); err != nil {
		return "", fmt.Errorf("cluster identity unavailable: %w", err)
	}
	if system.UID == "" {
		return "", fmt.Errorf("cluster identity unavailable")
	}
	return string(system.UID), nil
}

// ensurePlatformPolicy creates the scope's policy, or extends an existing one
// from the same package with any runtime profile or route it lacks. Existing
// entries, the selector and the ceilings are never rewritten here.
//
// With replace, a policy from another package is rewritten to this one.
func ensurePlatformPolicy(ctx context.Context, store client.Client, policy *api.CellnExecutionPolicy, packageHash string, replace bool) error {
	err := store.Create(ctx, policy)
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create %s: %w; partial installation retained, no run was submitted", policy.Name, err)
	}
	var existing api.CellnExecutionPolicy
	if err := store.Get(ctx, client.ObjectKeyFromObject(policy), &existing); err != nil {
		return err
	}
	if existing.Annotations[packageAnnotation] != packageHash {
		if !replace {
			return fmt.Errorf("%s exists from another package; one scope carries exactly one package", policy.Name)
		}
		policy.ResourceVersion = existing.ResourceVersion
		return store.Update(ctx, policy)
	}
	patch := client.MergeFrom(existing.DeepCopy())
	changed := false
	for _, ref := range policy.Spec.RuntimeProfiles {
		if !slices.ContainsFunc(existing.Spec.RuntimeProfiles, func(r api.CellnExecutionPolicyRuntime) bool { return r.Ref == ref.Ref }) {
			existing.Spec.RuntimeProfiles = append(existing.Spec.RuntimeProfiles, ref)
			changed = true
		}
	}
	for _, route := range policy.Spec.Routes {
		if !slices.ContainsFunc(existing.Spec.Routes, func(r api.CellnExecutionPolicyRoute) bool { return reflect.DeepEqual(r, route) }) {
			existing.Spec.Routes = append(existing.Spec.Routes, route)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return store.Patch(ctx, &existing, patch)
}

// ensurePlatformObject creates the object, or accepts an existing one that was
// published from the same package. Anything else is a conflict, replaced only
// when the operator approved moving the scope to this package.
func ensurePlatformObject(ctx context.Context, store client.Client, object client.Object, packageHash string, replace bool) error {
	err := store.Create(ctx, object)
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create %s: %w; partial installation retained, no run was submitted", object.GetName(), err)
	}
	existing := object.DeepCopyObject().(client.Object)
	if err := store.Get(ctx, client.ObjectKeyFromObject(object), existing); err != nil {
		return err
	}
	if existing.GetAnnotations()[packageAnnotation] != packageHash {
		if !replace {
			return fmt.Errorf("%s exists from another package; one scope carries exactly one package", object.GetName())
		}
		if object.GetNamespace() != "" {
			// A namespace's wrappers are rebound in place (rebindTenantWrappers):
			// deleting its Agent would take the runs that name it with it. Only
			// the package record moves.
			patch := client.MergeFrom(existing.DeepCopyObject().(client.Object))
			annotations := existing.GetAnnotations()
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[packageAnnotation] = packageHash
			existing.SetAnnotations(annotations)
			return store.Patch(ctx, existing, patch)
		}
		return replacePlatformObject(ctx, store, object, existing)
	}
	return nil
}
