package cellninstall

import (
	"context"
	"strings"
	"testing"

	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/internal/cellnplatform"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func extraStore(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = api.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func configureDaemonSet() *appsv1.DaemonSet {
	env := []corev1.EnvVar{
		{Name: "FLEET_SCOPE", Value: "ci"},
		{Name: "FLEET_PRINCIPAL", Value: "sympozium:celln"},
		{Name: "FLEET_PACKAGE_HASH", Value: "blake3:" + strings.Repeat("a", 64)},
		{Name: "FLEET_BACKENDS", Value: `[{"name":"native","provider":"llama-server","protocol":"openai-chat","endpoint":"http://h:8080/v1/chat/completions","model":"q","allowInsecure":true,"credentialFile":"/etc/celln-native/credentials/native"}]`},
	}
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: FleetConfigureDaemonSet, Namespace: fleetNamespace}}
	ds.Spec.Template.Spec.Containers = []corev1.Container{{Name: "configure", Env: env}}
	return ds
}

func TestFleetFactsAndExtraBackendsRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := extraStore(t, configureDaemonSet())
	facts, err := ReadFleetFacts(ctx, store)
	if err != nil || facts.Scope != "ci" || facts.Principal != "sympozium:celln" || len(facts.Backends) != 1 || facts.Backends[0] != "native" || facts.InstallBackends[0].Model != "q" {
		t.Fatalf("facts: %+v %v", facts, err)
	}
	if _, err := ReadFleetFacts(ctx, extraStore(t)); err == nil {
		t.Fatal("no fleet must be reported as such")
	}
	added := ExtraBackendFor(FleetBackend{Name: "claude", Model: FleetModel{Provider: "anthropic", Protocol: "anthropic-messages", Endpoint: "https://api.anthropic.com/v1/messages", Name: "claude-sonnet-5"}})
	if added.CredentialFile != "/etc/celln-native/credentials/claude" {
		t.Fatalf("credential path: %+v", added)
	}
	revision, err := AppendExtraBackend(ctx, store, facts, added)
	if err != nil || len(revision) != 16 {
		t.Fatalf("append: %q %v", revision, err)
	}
	list, states, err := ReadExtraBackends(ctx, store)
	if err != nil || len(list) != 1 || list[0].Name != "claude" || !strings.HasPrefix(states["claude"], "pending") {
		t.Fatalf("read back: %+v %+v %v", list, states, err)
	}
	if _, err := AppendExtraBackend(ctx, store, facts, added); err == nil {
		t.Fatal("a name already added must be refused")
	}
	if _, err := AppendExtraBackend(ctx, store, facts, ExtraBackend{Name: "native"}); err == nil {
		t.Fatal("a name configured at install must be refused")
	}
	if _, err := AppendExtraBackend(ctx, store, facts, ExtraBackend{Name: "Not-A-Label"}); err == nil {
		t.Fatal("a bad name must be refused")
	}
	second, err := AppendExtraBackend(ctx, store, facts, ExtraBackend{Name: "local", Provider: "llama-server"})
	if err != nil || second == revision {
		t.Fatalf("a second addition changes the revision: %q %v", second, err)
	}
	if err := RecordExtraBackendState(ctx, store, "claude", "ready"); err != nil {
		t.Fatal(err)
	}
	_, states, _ = ReadExtraBackends(ctx, store)
	if states["claude"] != "ready" || !strings.HasPrefix(states["local"], "pending") {
		t.Fatalf("states: %+v", states)
	}
	if err := RolloutConfigure(ctx, store, second); err != nil {
		t.Fatal(err)
	}
	var ds appsv1.DaemonSet
	_ = store.Get(ctx, types.NamespacedName{Namespace: fleetNamespace, Name: FleetConfigureDaemonSet}, &ds)
	if ds.Spec.Template.Annotations[extraRevisionAnnotation] != second {
		t.Fatalf("rollout annotation: %+v", ds.Spec.Template.Annotations)
	}
}

func TestPublishFleetBackendCredentialValueRules(t *testing.T) {
	ctx := context.Background()
	store := extraStore(t)
	keyed := FleetModel{Provider: ModelProviderAnthropic}
	if err := PublishFleetBackendCredentialValue(ctx, store, "claude", keyed, "short"); err == nil {
		t.Fatal("a short key must be refused")
	}
	key := "sk-ant-" + strings.Repeat("k", 40)
	if err := PublishFleetBackendCredentialValue(ctx, store, "claude", keyed, key); err != nil {
		t.Fatal(err)
	}
	if err := PublishFleetBackendCredentialValue(ctx, store, "claude", keyed, key); err != nil {
		t.Fatal("republishing the same key is fine")
	}
	if err := PublishFleetBackendCredentialValue(ctx, store, "claude", keyed, key+"x"); err == nil {
		t.Fatal("a different key for an existing backend must be refused")
	}
	if err := PublishFleetBackendCredentialValue(ctx, store, "local", FleetModel{Provider: ModelProviderLlamaServer}, ""); err != nil {
		t.Fatal(err)
	}
	var secret corev1.Secret
	_ = store.Get(ctx, types.NamespacedName{Namespace: fleetNamespace, Name: FleetModelCredentialSecret}, &secret)
	if string(secret.Data["claude"]) != key || string(secret.Data["local"]) != fleetModelPlaceholderCredential {
		t.Fatalf("secret: %v", secret.Data)
	}
}

func TestInstallNamespaceAndAuthoriseMode(t *testing.T) {
	ctx := context.Background()
	older := &api.AgentRuntime{ObjectMeta: metav1.ObjectMeta{Name: "celln-native", Namespace: "celln-agents", CreationTimestamp: metav1.NewTime(metav1.Now().Add(-1e9 * 60))}, Spec: api.AgentRuntimeSpec{CellnProfileRef: &api.CellnRuntimeProfileRef{Name: PlatformProfileName("ci", "native"), Revision: "v1"}}}
	newer := &api.AgentRuntime{ObjectMeta: metav1.ObjectMeta{Name: "celln-native", Namespace: "tenant-b", CreationTimestamp: metav1.Now()}, Spec: api.AgentRuntimeSpec{CellnProfileRef: &api.CellnRuntimeProfileRef{Name: PlatformProfileName("ci", "native"), Revision: "v1"}}}
	other := &api.AgentRuntime{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "x"}, Spec: api.AgentRuntimeSpec{Image: "img"}}
	ns, err := InstallNamespaceFor(ctx, extraStore(t, newer, older, other), "ci")
	if err != nil || ns != "celln-agents" {
		t.Fatalf("install namespace: %q %v", ns, err)
	}
	if ns, _ := InstallNamespaceFor(ctx, extraStore(t, other), "ci"); ns != "" {
		t.Fatalf("no wrapper means no namespace: %q", ns)
	}
	labeled := &api.CellnExecutionPolicy{Spec: api.CellnExecutionPolicySpec{NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{ScopeLabel: "ci"}}}}
	if AuthoriseModeOf(labeled) != cellnplatform.AuthoriseLabeled || AuthoriseModeOf(&api.CellnExecutionPolicy{}) != cellnplatform.AuthoriseAll || AuthoriseModeOf(nil) != cellnplatform.AuthoriseAll {
		t.Fatal("authorise mode must follow the policy's selector")
	}
}
