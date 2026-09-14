package apiserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	sympoziumv1alpha1 "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/internal/cellnplatform"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCellnPlatformProfilesAndWrappersFollowTheNamespacePolicy(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = sympoziumv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	raw := func(s string) apiextensionsv1.JSON { return apiextensionsv1.JSON{Raw: []byte(s)} }
	profile := &sympoziumv1alpha1.CellnRuntimeProfile{ObjectMeta: metav1.ObjectMeta{Name: "celln-native-trial"}, Spec: sympoziumv1alpha1.CellnRuntimeProfileSpec{Revision: "v1", Native: &sympoziumv1alpha1.CellnNativeProvisioning{CredentialProfile: "trial", SystemPrompt: "host persona", Template: raw(`{"model":"deepseek-chat","url":"https://api.deepseek.com/chat/completions"}`)}}}
	policy := &sympoziumv1alpha1.CellnExecutionPolicy{ObjectMeta: metav1.ObjectMeta{Name: "celln-fleet-trial"}, Spec: sympoziumv1alpha1.CellnExecutionPolicySpec{
		NamespaceSelector: cellnplatform.OpenSelector(cellnplatform.SystemNamespaces("sympozium-system")),
		RuntimeProfiles:   []sympoziumv1alpha1.CellnExecutionPolicyRuntime{{Ref: sympoziumv1alpha1.CellnRuntimeProfileRef{Name: profile.Name, Revision: "v1"}}},
		Routes:            []sympoziumv1alpha1.CellnExecutionPolicyRoute{{Provider: "deepseek", Protocol: "openai-chat", Models: []string{"deepseek-chat"}, EndpointOrigins: []string{"https://api.deepseek.com"}, Auth: "host-profile"}},
	}}
	ns := func(name string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{cellnplatform.NamespaceNameLabel: name}}}
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(profile, policy, ns("team-a"), ns("sympozium-system")).Build()
	srv := NewServer(cl, nil, nil, logr.Discard())
	get := func(namespace string) []CellnPlatformProfile {
		t.Helper()
		res := httptest.NewRecorder()
		srv.Handler(nil).ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/v1/celln-platform/profiles?namespace="+namespace, nil))
		if res.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", namespace, res.Code, res.Body.String())
		}
		var out []CellnPlatformProfile
		if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := get("team-a"); len(got) != 1 || got[0].Name != "celln-native-trial" || got[0].Provider != "deepseek" || got[0].Model != "deepseek-chat" || got[0].CredentialProfile != "trial" || got[0].Wrapper != "celln-native" || got[0].SystemPrompt != "host persona" {
		t.Fatalf("tenant profiles: %+v", got)
	}
	if got := get("sympozium-system"); len(got) != 0 {
		t.Fatalf("control-plane namespace offered profiles: %+v", got)
	}
	post := func(namespace, body string) *httptest.ResponseRecorder {
		res := httptest.NewRecorder()
		srv.Handler(nil).ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/api/v1/celln-platform/wrappers?namespace="+namespace, strings.NewReader(body)))
		return res
	}
	res := post("team-a", `{"profile":"celln-native-trial"}`)
	var wrappers cellnplatform.Wrappers
	if res.Code != http.StatusOK || json.Unmarshal(res.Body.Bytes(), &wrappers) != nil || wrappers.Connection != "celln-native" || len(wrappers.Created) != 3 {
		t.Fatalf("ensure: %d %s", res.Code, res.Body.String())
	}
	var connection sympoziumv1alpha1.ModelConnection
	if err := cl.Get(t.Context(), types.NamespacedName{Namespace: "team-a", Name: "celln-native"}, &connection); err != nil || connection.Spec.CredentialProfile != "trial" {
		t.Fatalf("wrapper connection: %v", err)
	}
	if res := post("team-a", `{"profile":"celln-native-trial"}`); res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"created":[]`) {
		t.Fatalf("second ensure: %d %s", res.Code, res.Body.String())
	}
	if res := post("sympozium-system", `{"profile":"celln-native-trial"}`); res.Code != http.StatusForbidden {
		t.Fatalf("excluded namespace prepared: %d %s", res.Code, res.Body.String())
	}
	if res := post("team-a", `{}`); res.Code != http.StatusBadRequest {
		t.Fatalf("empty profile accepted: %d", res.Code)
	}
}
