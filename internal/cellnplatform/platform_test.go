package cellnplatform

import (
	"context"
	"strings"
	"testing"

	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func store(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = api.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func catalogue(selector metav1.LabelSelector) (*api.CellnRuntimeProfile, *api.CellnExecutionPolicy) {
	raw := func(s string) apiextensionsv1.JSON { return apiextensionsv1.JSON{Raw: []byte(s)} }
	profile := &api.CellnRuntimeProfile{ObjectMeta: metav1.ObjectMeta{Name: "celln-native-trial", UID: "profile"}, Spec: api.CellnRuntimeProfileSpec{Revision: "v1", Native: &api.CellnNativeProvisioning{CredentialProfile: "trial", Template: raw(`{"contract":"celln.json-tools/v1","model":"deepseek-chat","url":"https://api.deepseek.com/chat/completions"}`)}}}
	policy := &api.CellnExecutionPolicy{ObjectMeta: metav1.ObjectMeta{Name: "celln-fleet-trial", UID: "policy"}, Spec: api.CellnExecutionPolicySpec{
		NamespaceSelector: selector,
		RuntimeProfiles:   []api.CellnExecutionPolicyRuntime{{Ref: api.CellnRuntimeProfileRef{Name: profile.Name, Revision: "v1"}}},
		Routes:            []api.CellnExecutionPolicyRoute{{Provider: "deepseek", Protocol: "openai-chat", Models: []string{"deepseek-chat"}, EndpointOrigins: []string{"https://api.deepseek.com"}, Auth: "host-profile"}},
	}}
	return profile, policy
}

func namespace(name string, labels map[string]string) *corev1.Namespace {
	if labels == nil {
		labels = map[string]string{}
	}
	labels[NamespaceNameLabel] = name
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-uid"), Labels: labels}}
}

func TestOpenSelectorAdmitsOrdinaryNamespacesAndRefusesSystemAndExcluded(t *testing.T) {
	ctx := context.Background()
	profile, policy := catalogue(OpenSelector(SystemNamespaces("sympozium-system", "celln-system")))
	c := store(t, profile, policy,
		namespace("team-a", nil),
		namespace("kube-system", nil),
		namespace("sympozium-system", nil),
		namespace("fenced", map[string]string{ExcludedLabel: "true"}),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "unstamped", UID: "u"}}, // no metadata.name label
	)
	for name, want := range map[string]int{"team-a": 1, "unstamped": 1, "kube-system": 0, "sympozium-system": 0, "fenced": 0} {
		got, err := AuthorisedProfiles(ctx, c, name)
		if err != nil || len(got) != want {
			t.Fatalf("%s: %d profiles (want %d) %v", name, len(got), want, err)
		}
	}
	if _, err := Selector("bogus", "trial", nil); err == nil {
		t.Fatal("unknown authorisation mode accepted")
	}
	strict, _ := Selector(AuthoriseLabeled, "trial", nil)
	if strict.MatchLabels[ScopeLabel] != "trial" {
		t.Fatalf("strict selector: %+v", strict)
	}
}

func TestEnsureWrappersCreatesMissingObjectsOnceAndNeverOverwrites(t *testing.T) {
	ctx := context.Background()
	profile, policy := catalogue(OpenSelector(SystemNamespaces()))
	existing := &api.ModelConnection{ObjectMeta: metav1.ObjectMeta{Name: WrapperConnectionName, Namespace: "team-a"}, Spec: api.ModelConnectionSpec{Provider: "custom", Protocol: "openai-chat", Endpoint: "https://tenant.example/v1", CredentialProfile: "tenant", Models: []string{"deepseek-chat"}}}
	c := store(t, profile, policy, namespace("team-a", nil), existing)
	got, err := EnsureWrappers(ctx, c, "team-a", profile.Name)
	if err != nil || got.Runtime != WrapperRuntimeName || got.Agent != WrapperAgentName || got.Connection != WrapperConnectionName || strings.Join(got.Created, ",") != "celln-native,celln-agent" {
		t.Fatalf("first ensure: %+v %v", got, err)
	}
	var runtime api.AgentRuntime
	if err := c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: WrapperRuntimeName}, &runtime); err != nil || runtime.Spec.CellnProfileRef == nil || runtime.Spec.CellnProfileRef.Name != profile.Name || runtime.Labels[ManagedByLabel] != ManagedByValue {
		t.Fatalf("runtime wrapper: %+v %v", runtime.Spec, err)
	}
	var connection api.ModelConnection
	if err := c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: WrapperConnectionName}, &connection); err != nil || connection.Spec.CredentialProfile != "tenant" {
		t.Fatalf("tenant's own connection was overwritten: %+v", connection.Spec)
	}
	again, err := EnsureWrappers(ctx, c, "team-a", profile.Name)
	if err != nil || len(again.Created) != 0 {
		t.Fatalf("second ensure not idempotent: %+v %v", again, err)
	}
	if _, err := EnsureWrappers(ctx, c, "team-a", "other-profile"); err == nil {
		t.Fatal("unauthorised profile prepared")
	}
	if _, err := EnsureWrappers(ctx, c, "kube-system", profile.Name); err == nil {
		t.Fatal("system namespace prepared")
	}
	// A fresh namespace gets the connection bound to the policy's route.
	if err := c.Create(ctx, namespace("team-b", nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureWrappers(ctx, c, "team-b", profile.Name); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "team-b", Name: WrapperConnectionName}, &connection); err != nil || connection.Spec.Provider != "deepseek" || connection.Spec.CredentialProfile != "trial" || connection.Spec.Endpoint != "https://api.deepseek.com/chat/completions" || connection.Spec.Validate() != nil {
		t.Fatalf("connection not bound to the policy route: %+v %v", connection.Spec, err)
	}
}

// An operator-approved insecure profile yields a tenant connection that
// carries the approval and binds the policy's plain-HTTP route.
func TestTenantWrappersCarryInsecureApprovalFromProfile(t *testing.T) {
	raw := func(s string) apiextensionsv1.JSON { return apiextensionsv1.JSON{Raw: []byte(s)} }
	profile := &api.CellnRuntimeProfile{ObjectMeta: metav1.ObjectMeta{Name: "celln-native-local"}, Spec: api.CellnRuntimeProfileSpec{Revision: "v1", Native: &api.CellnNativeProvisioning{CredentialProfile: "local", Template: raw(`{"model":"qwen.gguf","url":"http://100.81.163.75:8080/v1/chat/completions","allow_insecure":true}`)}}}
	policy := &api.CellnExecutionPolicy{ObjectMeta: metav1.ObjectMeta{Name: "celln-fleet-local"}, Spec: api.CellnExecutionPolicySpec{Routes: []api.CellnExecutionPolicyRoute{{Provider: "llama-server", Protocol: "openai-chat", Models: []string{"qwen.gguf"}, EndpointOrigins: []string{"http://100.81.163.75:8080"}, Auth: "host-profile"}}}}
	objects, err := TenantWrappers("team-a", profile, policy)
	if err != nil {
		t.Fatal(err)
	}
	connection := objects[2].(*api.ModelConnection)
	if !connection.Spec.AllowInsecure || connection.Spec.Provider != "llama-server" || connection.Spec.Validate() != nil {
		t.Fatalf("insecure approval not carried: %+v %v", connection.Spec, connection.Spec.Validate())
	}
	profile.Spec.Native.Template = raw(`{"model":"qwen.gguf","url":"http://100.81.163.75:8080/v1/chat/completions"}`)
	if _, err := TenantWrappers("team-a", profile, policy); err == nil {
		t.Fatal("plain-HTTP profile without operator approval produced a connection")
	}
}
