package cellninstall

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
}

// PlatformCatalogueNames are the cluster-scoped objects one scope publishes.
func PlatformCatalogueNames(scope string) (profile, policy string, tool func(string) string) {
	return "celln-native-" + scope, "celln-fleet-" + scope, func(name string) string { return "celln-" + scope + "-" + name }
}

// InstallPlatform publishes the starter configuration as CellnRuntimeProfile,
// ClusterCellnTools and a CellnExecutionPolicy selecting namespaces by
// ScopeLabel, then labels the target namespace and creates its wrapper objects
// (AgentRuntime, Agent, ModelConnection). It writes a platform-mode
// registrations.json and a sample run. Existing catalogue objects are accepted
// only when they carry the same package; nothing is replaced and no run is
// submitted.
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
	var cat catalogue
	var configured receipt
	var native cellnparent.NativeProvisionConfig
	hashes := map[string]string{}
	for file, target := range map[string]any{"catalogue.json": &cat, "configured.json": &configured, "native-template.json": &native} {
		hash, err := read(filepath.Join(o.ConfigurationDir, file), target)
		if err != nil {
			return err
		}
		hashes[file] = hash
	}
	if configured.PackageHash != o.PackageHash || configured.CatalogueHash != hashes["catalogue.json"] || configured.NativeTemplateHash != hashes["native-template.json"] {
		return fmt.Errorf("configuration differs from reviewed package/receipt")
	}
	if configured.APIVersion != "celln.native-starter-configured/v1" || configured.ExecutionAuthorized || configured.Readiness != "not_established" || native.ModelProfile != configured.ModelProfile || configured.Principal != o.Principal || len(cat.Tools) != 3 || cat.Worker.ContractVersion != "celln.json-tools/v1" {
		return fmt.Errorf("operator starter configuration mismatch")
	}
	var harness struct {
		Contract string `json:"contract"`
		System   string `json:"system"`
		Model    string `json:"model"`
		URL      string `json:"url"`
	}
	if json.Unmarshal(native.Template, &harness) != nil || harness.Contract != "celln.json-tools/v1" || harness.System != cat.SystemPrompt || harness.Model != configured.Model.Model {
		return fmt.Errorf("native template does not match the starter catalogue")
	}
	origin, err := api.ModelEndpointOriginInsecure(harness.URL, true)
	if err != nil {
		return err
	}
	protocol := configured.Model.Protocol
	if protocol == "" {
		protocol = "openai-chat"
	}
	var worker struct {
		Capabilities struct {
			TimeoutMs int64 `json:"timeoutMs"`
		} `json:"capabilities"`
	}
	if json.Unmarshal(native.Worker, &worker) != nil || worker.Capabilities.TimeoutMs < 1000 {
		return fmt.Errorf("native worker request lacks a bounded lifetime")
	}
	profileName, policyName, toolName := PlatformCatalogueNames(o.Scope)
	meta := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Labels: map[string]string{"app.kubernetes.io/part-of": "sympozium"}, Annotations: map[string]string{packageAnnotation: configured.PackageHash}}
	}
	profile := &api.CellnRuntimeProfile{ObjectMeta: meta(profileName), Spec: api.CellnRuntimeProfileSpec{
		Revision: cat.Worker.Revision, ContractVersion: cat.Worker.ContractVersion, Executable: cat.Worker.Executable, Closure: cat.Worker.Closure, Mote: cat.Worker.Mote, PublisherKey: cat.Worker.PublisherKey, EntryPoint: cat.Worker.EntryPoint, Platform: cat.Worker.Platform, Lane: cat.Worker.Lane,
		Lifecycles: []string{"disposable-one-shot", "enduring"}, Limits: cat.Worker.Limits, JSON: cat.Worker.JSON.DeepCopy(),
		Native: &api.CellnNativeProvisioning{AdmissionWindowMs: int64(native.AdmissionWindowMs), Parent: apiextensionsv1.JSON{Raw: native.Parent}, Worker: apiextensionsv1.JSON{Raw: native.Worker}, Template: apiextensionsv1.JSON{Raw: native.Template}, ModelProfile: native.ModelProfile, CredentialProfile: o.Scope, ReservedMemoryBytes: int64(native.ReservedMemoryBytes), TurnModelRequests: int64(native.TurnModelRequests), TurnOutputTokens: int64(native.TurnOutputTokens), SystemPrompt: cat.SystemPrompt},
	}}
	objects := []client.Object{profile}
	policyTools := make([]api.CellnExecutionPolicyTool, 0, len(cat.Tools))
	clusterRefs := make([]api.ClusterCellnToolRef, 0, len(cat.Tools))
	for _, entry := range cat.Tools {
		objects = append(objects, &api.ClusterCellnTool{ObjectMeta: meta(toolName(entry.Name)), Spec: entry.Spec})
		policyTools = append(policyTools, api.CellnExecutionPolicyTool{Ref: api.ClusterCellnToolRef{Name: toolName(entry.Name), Revision: entry.Spec.Revision}})
		clusterRefs = append(clusterRefs, api.ClusterCellnToolRef{Name: toolName(entry.Name), Revision: entry.Spec.Revision})
	}
	limits := configured.HostLimits
	selector, err := cellnplatform.Selector(o.Authorise, o.Scope, cellnplatform.SystemNamespaces(o.ControllerNamespace, "celln-system"))
	if err != nil {
		return err
	}
	policy := &api.CellnExecutionPolicy{ObjectMeta: meta(policyName), Spec: api.CellnExecutionPolicySpec{
		NamespaceSelector: selector,
		RuntimeProfiles:   []api.CellnExecutionPolicyRuntime{{Ref: api.CellnRuntimeProfileRef{Name: profileName, Revision: cat.Worker.Revision}}},
		Tools:             policyTools,
		Lifecycles:        []string{"direct-one-shot", "harness-one-shot", "enduring"},
		Routes:            []api.CellnExecutionPolicyRoute{{Provider: configured.Model.Provider, Protocol: protocol, Models: []string{configured.Model.Model}, EndpointOrigins: []string{origin}, Auth: "host-profile", AllowInsecure: configured.Model.AllowInsecure}},
		Ceilings:          api.CellnExecutionPolicyCeilings{MaxTurns: int64(limits.MaxTurns), MaxModelRequests: int64(limits.MaxModelRequests), MaxOutputTokens: limits.MaxOutputTokens, MaxParentLeaseSeconds: int64(limits.LeaseSeconds), MaxTurnSeconds: worker.Capabilities.TimeoutMs / 1000},
	}}
	objects = append(objects, policy)
	// Reserve the private output before any cluster change.
	if err := os.Mkdir(o.OutputDir, 0700); err != nil {
		return err
	}
	for _, object := range objects {
		if err := ensurePlatformObject(ctx, store, object, configured.PackageHash); err != nil {
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
	// creates on demand for any other authorised namespace.
	wrappers, err := cellnplatform.TenantWrappers(o.Namespace, profile, policy)
	if err != nil {
		return err
	}
	for _, object := range wrappers {
		annotations := object.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[packageAnnotation] = configured.PackageHash
		object.SetAnnotations(annotations)
		if err := ensurePlatformObject(ctx, store, object, configured.PackageHash); err != nil {
			return err
		}
	}
	write := func(name string, value any) error {
		raw, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return err
		}
		f, err := os.OpenFile(filepath.Join(o.OutputDir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
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
	run := &api.AgentRun{TypeMeta: metav1.TypeMeta{APIVersion: "sympozium.ai/v1alpha1", Kind: "AgentRun"}, ObjectMeta: metav1.ObjectMeta{GenerateName: "celln-starter-", Namespace: o.Namespace}, Spec: api.AgentRunSpec{
		AgentRef: "celln-agent", Backend: "celln", ExecutionLifecycle: "enduring", SystemPrompt: cat.SystemPrompt, Cleanup: "delete",
		Model:          api.ModelSpec{ConnectionRef: "celln-native", Model: configured.Model.Model},
		CellnSelection: &api.CellnCatalogueSelection{RuntimeRef: "celln-native", ToolRefs: []api.CellnCatalogueToolRef{}, ClusterToolRefs: clusterRefs},
		Enduring:       &api.EnduringRunSpec{LeaseSeconds: min(600, limits.LeaseSeconds), MaxTurns: min(8, limits.MaxTurns), MaxModelRequests: min(24, limits.MaxModelRequests), MaxOutputTokens: min(8192, limits.MaxOutputTokens)},
		Task:           api.NewStringTask("Write violet to notes.txt using workspace-write with revision 0."),
	}}
	if err := write("run.json", run); err != nil {
		return err
	}
	return write("installed.json", map[string]any{"namespace": o.Namespace, "scope": o.Scope, "packageHash": configured.PackageHash, "profile": profileName, "policy": policyName, "runSubmitted": false, "readiness": "not_established"})
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

// ensurePlatformObject creates the object, or accepts an existing one that was
// published from the same package. Anything else is a conflict, never replaced.
func ensurePlatformObject(ctx context.Context, store client.Client, object client.Object, packageHash string) error {
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
		return fmt.Errorf("%s exists from another package; one scope carries exactly one package", object.GetName())
	}
	return nil
}
