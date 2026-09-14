package cellninstall

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/internal/cellnparent"
	"github.com/zeebo/blake3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// starterConfiguration materializes the reviewed testdata with a consistent
// receipt, as the fleet nodes publish it.
func starterConfiguration(t *testing.T) (dir, packageHash, principal string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "configuration")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	var metadata receipt
	for _, name := range []string{"catalogue.json", "native-template.json", "configured.json"} {
		raw, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		raw = []byte(strings.TrimSuffix(string(raw), "\n"))
		if name == "configured.json" {
			if err := json.Unmarshal(raw, &metadata); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for file, field := range map[string]*string{"catalogue.json": &metadata.CatalogueHash, "native-template.json": &metadata.NativeTemplateHash} {
		raw, _ := os.ReadFile(filepath.Join(dir, file))
		*field = fmt.Sprintf("blake3:%x", blake3.Sum256(raw))
	}
	raw, _ := json.Marshal(metadata)
	if err := os.WriteFile(filepath.Join(dir, "configured.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return dir, metadata.PackageHash, metadata.Principal
}

func platformInstallStore(t *testing.T) client.Client {
	t.Helper()
	replicas := int32(1)
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "sympozium-controller-manager", Namespace: "sympozium-system", Generation: 1}, Spec: appsv1.DeploymentSpec{Replicas: &replicas, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "manager"}}}}}, Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}}
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "cluster-uid"}}, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-a"}}, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-b"}}).Build()
}

func TestInstallPlatformPublishesCatalogueOncePerScopeAndWrapsNamespaces(t *testing.T) {
	ctx := context.Background()
	dir, packageHash, principal := starterConfiguration(t)
	store := platformInstallStore(t)
	clusterID, err := ClusterIdentity(ctx, store)
	if err != nil || clusterID != "cluster-uid" {
		t.Fatalf("cluster identity: %q %v", clusterID, err)
	}
	options := func(namespace string) PlatformOptions {
		return PlatformOptions{Namespace: namespace, ConfigurationDir: dir, OutputDir: filepath.Join(t.TempDir(), "out-"+namespace), Scope: "trial", ClusterID: clusterID, PackageHash: packageHash, Principal: principal, ControllerNamespace: "sympozium-system"}
	}
	first := options("tenant-a")
	if err := InstallPlatform(ctx, store, first); err != nil {
		t.Fatal(err)
	}
	profileName, policyName, toolName := PlatformCatalogueNames("trial")
	var profile api.CellnRuntimeProfile
	if err := store.Get(ctx, types.NamespacedName{Name: profileName}, &profile); err != nil {
		t.Fatal(err)
	}
	native := profile.Spec.Native
	if native == nil || native.CredentialProfile != "trial" || native.ModelProfile == "" || native.SystemPrompt == "" || len(native.Parent.Raw) == 0 || len(native.Template.Raw) == 0 || profile.Spec.Lifecycles[1] != "enduring" || profile.Annotations[packageAnnotation] != packageHash {
		t.Fatalf("profile lacks native provisioning material: %+v", profile.Spec)
	}
	var policy api.CellnExecutionPolicy
	if err := store.Get(ctx, types.NamespacedName{Name: policyName}, &policy); err != nil {
		t.Fatal(err)
	}
	route := policy.Spec.Routes[0]
	if policy.Spec.NamespaceSelector.MatchLabels[ScopeLabel] != "trial" || len(policy.Spec.Tools) != 3 || route.Auth != "host-profile" || route.Provider != "deepseek" || route.EndpointOrigins[0] != "https://api.deepseek.com" || policy.Spec.Ceilings.MaxTurns != 12 || policy.Spec.Ceilings.MaxParentLeaseSeconds != 3600 || policy.Spec.Ceilings.MaxTurnSeconds != 60 {
		t.Fatalf("policy does not bind the reviewed route and ceilings: %+v", policy.Spec)
	}
	var tool api.ClusterCellnTool
	if err := store.Get(ctx, types.NamespacedName{Name: toolName("https-fetch")}, &tool); err != nil || tool.Spec.Revision != "v1" {
		t.Fatalf("cluster tool missing: %v", err)
	}
	var namespace corev1.Namespace
	if err := store.Get(ctx, types.NamespacedName{Name: "tenant-a"}, &namespace); err != nil || namespace.Labels[ScopeLabel] != "trial" {
		t.Fatalf("tenant namespace not authorised: %v %v", err, namespace.Labels)
	}
	var wrapper api.AgentRuntime
	var connection api.ModelConnection
	if err := store.Get(ctx, types.NamespacedName{Namespace: "tenant-a", Name: "celln-native"}, &wrapper); err != nil || wrapper.Spec.CellnProfileRef == nil || wrapper.Spec.CellnProfileRef.Name != profileName || wrapper.Spec.Celln != nil {
		t.Fatalf("tenant wrapper: %v %+v", err, wrapper.Spec)
	}
	if err := store.Get(ctx, types.NamespacedName{Namespace: "tenant-a", Name: "celln-native"}, &connection); err != nil || connection.Spec.CredentialProfile != "trial" || connection.Spec.SecretRef != "" || connection.Spec.Validate() != nil {
		t.Fatalf("tenant model connection: %v %+v", err, connection.Spec)
	}
	var grants corev1.ConfigMapList
	if err := store.List(ctx, &grants, client.InNamespace("tenant-a")); err != nil || len(grants.Items) != 0 {
		t.Fatal("platform installation published namespace grant ConfigMaps")
	}
	var tools api.CellnToolList
	if err := store.List(ctx, &tools, client.InNamespace("tenant-a")); err != nil || len(tools.Items) != 0 {
		t.Fatal("platform installation copied namespaced tools")
	}
	out := first.OutputDir
	raw, err := os.ReadFile(filepath.Join(out, "registrations.json"))
	if err != nil {
		t.Fatal(err)
	}
	var registration cellnparent.RegistrationConfig
	if err := json.Unmarshal(raw, &registration); err != nil || registration.Platform == nil || registration.Platform.ClusterID != clusterID || registration.Platform.Target != ManagedRouterURL || registration.LocalProvisioner != nil || registration.Journal != FleetJournalRoot+"/journal" {
		t.Fatalf("registrations are not platform-mode: %v %+v", err, registration)
	}
	raw, _ = os.ReadFile(filepath.Join(out, "run.json"))
	var run api.AgentRun
	if err := json.Unmarshal(raw, &run); err != nil || run.Spec.Model.ConnectionRef != "celln-native" || len(run.Spec.CellnSelection.ClusterToolRefs) != 3 || len(run.Spec.CellnSelection.ToolRefs) != 0 || run.Spec.SystemPrompt != native.SystemPrompt || run.Spec.Enduring.MaxTurns != 8 || run.Spec.ExecutionLifecycle != "enduring" {
		t.Fatalf("sample run is not a platform run: %v %+v", err, run.Spec)
	}
	values, err := ConfigureFleet(ctx, store, Options{OutputDir: out, ControllerNamespace: "sympozium-system", OwnerTarget: ManagedRouterURL})
	if err != nil || values[0] != "celln.fleet.parentConfigSecret="+FleetParentConfigSecret {
		t.Fatalf("fleet wiring from platform registration: %v %v", err, values)
	}
	// A second namespace reuses the scope's catalogue and gets only wrappers.
	if err := InstallPlatform(ctx, store, options("tenant-b")); err != nil {
		t.Fatalf("second namespace on the same scope: %v", err)
	}
	if err := store.Get(ctx, types.NamespacedName{Namespace: "tenant-b", Name: "celln-native"}, &wrapper); err != nil {
		t.Fatal(err)
	}
	// A different package under the same scope is refused before tenant changes.
	mismatch := options("tenant-b")
	mismatch.Principal = "someone-else"
	if err := InstallPlatform(ctx, store, mismatch); err == nil {
		t.Fatal("principal differing from the reviewed configuration accepted")
	}
}
