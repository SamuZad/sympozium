package cellnauthority

import (
	"context"
	"strings"
	"testing"
	"time"

	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/internal/modelconnection"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// An owner-installed credential profile is an operator route: policy must say
// host-profile and the runtime profile's native material must name it.
func TestPlatformResolverBindsHostProfileRouteToRuntimeNative(t *testing.T) {
	f := newPlatformFixture(t, "tenant", true)
	ctx := context.Background()
	request := PlatformResolveRequest{ClusterID: "cluster", Now: f.now, AdmissionWindow: 60 * time.Second}
	var connection api.ModelConnection
	if err := f.client.Get(ctx, types.NamespacedName{Namespace: "tenant", Name: "model"}, &connection); err != nil {
		t.Fatal(err)
	}
	connection.Spec.SecretRef = ""
	connection.Spec.CredentialProfile = "starter"
	if err := f.client.Update(ctx, &connection); err != nil {
		t.Fatal(err)
	}
	setPolicyAuth := func(auth string) {
		t.Helper()
		var policies api.CellnExecutionPolicyList
		if err := f.client.List(ctx, &policies); err != nil {
			t.Fatal(err)
		}
		for i := range policies.Items {
			policies.Items[i].Spec.Routes[0].Auth = auth
			if err := f.client.Update(ctx, &policies.Items[i]); err != nil {
				t.Fatal(err)
			}
		}
	}
	setPolicyAuth("host-profile")
	if _, err := f.resolver.Resolve(ctx, f.runKey, request); PlatformReason(err) != ReasonRouteMismatch {
		t.Fatalf("profile without native material accepted a host credential: %v", err)
	}
	var profile api.CellnRuntimeProfile
	if err := f.client.Get(ctx, types.NamespacedName{Name: "json-agent-v1"}, &profile); err != nil {
		t.Fatal(err)
	}
	raw := func(s string) apiextensionsv1.JSON { return apiextensionsv1.JSON{Raw: []byte(s)} }
	profile.Spec.Native = &api.CellnNativeProvisioning{AdmissionWindowMs: 60000, Parent: raw(`{"workload":{"caller":"sympozium:celln"}}`), Worker: raw(`{"workload":{"caller":"sympozium:celln"}}`), Template: raw(`{"contract":"celln.json-tools/v1"}`), ModelProfile: "blake3:" + strings.Repeat("c", 64), CredentialProfile: "starter", ReservedMemoryBytes: 1, TurnModelRequests: 3, TurnOutputTokens: 1536}
	if err := f.client.Update(ctx, &profile); err != nil {
		t.Fatal(err)
	}
	resolution, err := f.resolver.Resolve(ctx, f.runKey, request)
	if err != nil {
		t.Fatal(err)
	}
	route := resolution.Decision.Route
	if route.Auth != "host-profile" || route.CredentialSource != nil || route.CredentialSourceRef != nil || route.Model != "gpt-test" || route.EndpointOrigin != "https://model.example" || f.spy.secretReads != 0 {
		t.Fatalf("host-profile route not bound without secrets: %+v", route)
	}
	if err := f.resolver.Revalidate(ctx, f.runKey, *resolution); err != nil {
		t.Fatalf("stable host-profile route failed revalidation: %v", err)
	}
	// The per-turn cap is the profile's reviewed allowance, bounded by the run.
	if cap := resolution.Decision.Budget.TurnCap; cap.Requests != 3 || cap.OutputTokens != 1536 {
		t.Fatalf("turn cap does not follow the native per-turn allowance: %+v", cap)
	}
	// The controller freezes the connection's route into spec.model; a mirror is
	// not an override, a different profile is.
	var run api.AgentRun
	if err := f.client.Get(ctx, f.runKey, &run); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Get(ctx, types.NamespacedName{Namespace: "tenant", Name: "model"}, &connection); err != nil {
		t.Fatal(err)
	}
	run.Spec.Model.Provider, run.Spec.Model.Protocol, run.Spec.Model.BaseURL, run.Spec.Model.CredentialProfile = "openai", "openai-chat", "https://model.example/v1/chat/completions", "starter"
	run.Spec.Model.ConnectionRevision = modelconnection.Revision(&connection)
	if err := f.client.Update(ctx, &run); err != nil {
		t.Fatal(err)
	}
	if _, err := f.resolver.Resolve(ctx, f.runKey, request); err != nil {
		t.Fatalf("mirrored connection route treated as an override: %v", err)
	}
	run.Spec.Model.ConnectionRevision = "sha256:" + strings.Repeat("0", 64)
	if err := f.client.Update(ctx, &run); err != nil {
		t.Fatal(err)
	}
	if _, err := f.resolver.Resolve(ctx, f.runKey, request); PlatformReason(err) != ReasonRouteMismatch {
		t.Fatalf("stale pinned connection revision accepted: %v", err)
	}
	run.Spec.Model.ConnectionRevision = ""
	run.Spec.Model.CredentialProfile = "other"
	if err := f.client.Update(ctx, &run); err != nil {
		t.Fatal(err)
	}
	if _, err := f.resolver.Resolve(ctx, f.runKey, request); PlatformReason(err) != ReasonRouteMismatch {
		t.Fatalf("foreign credential profile on the run accepted: %v", err)
	}
	run.Spec.Model.CredentialProfile = "starter"
	if err := f.client.Update(ctx, &run); err != nil {
		t.Fatal(err)
	}
	// An operator-approved plain-HTTP endpoint (a LAN llama-server) is allowed
	// for a node-held credential and never for a cluster Secret.
	insecure := func(endpoint string, allow bool, origin string, routeApproved bool) {
		t.Helper()
		if err := f.client.Get(ctx, types.NamespacedName{Namespace: "tenant", Name: "model"}, &connection); err != nil {
			t.Fatal(err)
		}
		connection.Spec.Endpoint, connection.Spec.AllowInsecure = endpoint, allow
		if err := f.client.Update(ctx, &connection); err != nil {
			t.Fatal(err)
		}
		var policies api.CellnExecutionPolicyList
		if err := f.client.List(ctx, &policies); err != nil {
			t.Fatal(err)
		}
		for i := range policies.Items {
			policies.Items[i].Spec.Routes[0].EndpointOrigins = []string{origin}
			policies.Items[i].Spec.Routes[0].AllowInsecure = routeApproved
			if err := f.client.Update(ctx, &policies.Items[i]); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.client.Get(ctx, f.runKey, &run); err != nil {
			t.Fatal(err)
		}
		run.Spec.Model.BaseURL, run.Spec.Model.AllowInsecure, run.Spec.Model.ConnectionRevision = "", false, ""
		if err := f.client.Update(ctx, &run); err != nil {
			t.Fatal(err)
		}
	}
	insecure("http://10.1.2.3:8080/v1/chat/completions", true, "http://10.1.2.3:8080", false)
	if _, err := f.resolver.Resolve(ctx, f.runKey, request); PlatformReason(err) != ReasonRouteMismatch {
		t.Fatalf("plain-HTTP endpoint admitted without the policy route's approval: %v", err)
	}
	insecure("http://10.1.2.3:8080/v1/chat/completions", true, "http://10.1.2.3:8080", true)
	if resolved, err := f.resolver.Resolve(ctx, f.runKey, request); err != nil || resolved.Decision.Route.EndpointOrigin != "http://10.1.2.3:8080" {
		t.Fatalf("approved insecure host-profile endpoint refused: %v", err)
	}
	setPolicyAuth("secret")
	if _, err := f.resolver.Resolve(ctx, f.runKey, request); PlatformReason(err) != ReasonRouteMismatch {
		t.Fatalf("policy requiring Secret custody accepted a host credential: %v", err)
	}
	setPolicyAuth("host-profile")
	connection.Spec.CredentialProfile = "other"
	if err := f.client.Get(ctx, types.NamespacedName{Namespace: "tenant", Name: "model"}, &connection); err != nil {
		t.Fatal(err)
	}
	connection.Spec.CredentialProfile = "other"
	if err := f.client.Update(ctx, &connection); err != nil {
		t.Fatal(err)
	}
	if _, err := f.resolver.Resolve(ctx, f.runKey, request); PlatformReason(err) != ReasonRouteMismatch {
		t.Fatalf("credential profile foreign to the runtime accepted: %v", err)
	}
}

// A default-open policy admits an unlabeled namespace and refuses one that
// opted out; the resolver evaluates the same selector the platform publishes.
func TestPlatformResolverHonoursOpenSelectorExclusions(t *testing.T) {
	f := newPlatformFixture(t, "tenant", true)
	ctx := context.Background()
	request := PlatformResolveRequest{ClusterID: "cluster", Now: f.now, AdmissionWindow: 60 * time.Second}
	var policies api.CellnExecutionPolicyList
	if err := f.client.List(ctx, &policies); err != nil {
		t.Fatal(err)
	}
	for i := range policies.Items {
		policies.Items[i].Spec.NamespaceSelector = metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "kubernetes.io/metadata.name", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"kube-system"}},
			{Key: "celln.sympozium.ai/excluded", Operator: metav1.LabelSelectorOpDoesNotExist},
		}}
		if err := f.client.Update(ctx, &policies.Items[i]); err != nil {
			t.Fatal(err)
		}
	}
	var ns corev1.Namespace
	if err := f.client.Get(ctx, types.NamespacedName{Name: "tenant"}, &ns); err != nil {
		t.Fatal(err)
	}
	ns.Labels = map[string]string{"kubernetes.io/metadata.name": "tenant"}
	if err := f.client.Update(ctx, &ns); err != nil {
		t.Fatal(err)
	}
	if _, err := f.resolver.Resolve(ctx, f.runKey, request); err != nil {
		t.Fatalf("open policy refused an ordinary namespace: %v", err)
	}
	ns.Labels["celln.sympozium.ai/excluded"] = "true"
	if err := f.client.Update(ctx, &ns); err != nil {
		t.Fatal(err)
	}
	if _, err := f.resolver.Resolve(ctx, f.runKey, request); PlatformReason(err) != ReasonPolicyWithdrawn {
		t.Fatalf("excluded namespace admitted: %v", err)
	}
}
