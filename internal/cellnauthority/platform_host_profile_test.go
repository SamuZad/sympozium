package cellnauthority

import (
	"context"
	"strings"
	"testing"
	"time"

	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/internal/modelconnection"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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
	profile.Spec.Native = &api.CellnNativeProvisioning{AdmissionWindowMs: 60000, Parent: raw(`{"workload":{"caller":"sympozium:celln"}}`), Worker: raw(`{"workload":{"caller":"sympozium:celln"}}`), Template: raw(`{"contract":"celln.json-tools/v1"}`), ModelProfile: "blake3:" + strings.Repeat("c", 64), CredentialProfile: "starter", ReservedMemoryBytes: 1, TurnModelRequests: 1, TurnOutputTokens: 1}
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
