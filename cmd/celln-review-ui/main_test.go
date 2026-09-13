package main

import (
	"context"
	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"net/http"
	"net/http/httptest"
	"os"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"strings"
	"testing"
)

func fixture(t *testing.T, objects ...client.Object) *review {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	path := t.TempDir() + "/token"
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 48)), 0600); err != nil {
		t.Fatal(err)
	}
	return &review{kube: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(), tokenFile: path, templates: map[string]*api.AgentRun{"model": {ObjectMeta: metav1.ObjectMeta{Namespace: "celln-review-a-495"}, Spec: api.AgentRunSpec{Backend: "celln", Task: api.NewStringTask("original")}}}, handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(202) })}
}
func call(r *review, method, path, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
func TestReviewRoutesFailClosed(t *testing.T) {
	r := fixture(t)
	for _, path := range []string{"/api/v1/review/runs", "/api/v1/runs/x?namespace=celln-review-a-495", "/ws"} {
		if got := call(r, "GET", path, "", "wrong").Code; got != 401 {
			t.Fatalf("%s: %d", path, got)
		}
	}
	for _, path := range []string{"/api/v1/secrets", "/api/v1/runs", "/api/v1/runs/x?namespace=default", "/ws"} {
		if got := call(r, "POST", path, "{}", strings.Repeat("x", 48)).Code; got != 403 {
			t.Fatalf("%s: %d", path, got)
		}
	}
	if err := os.WriteFile(r.tokenFile, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if got := call(r, "GET", "/api/v1/review/runs", "", " ").Code; got != 401 {
		t.Fatalf("empty token opened API: %d", got)
	}
}
func TestCreatePinsIdentityAndRejectsAuthority(t *testing.T) {
	r := fixture(t)
	token := strings.Repeat("x", 48)
	body := `{"mode":"model","task":"hello","requestId":"11111111111111111111111111111111"}`
	for i := 0; i < 2; i++ {
		if w := call(r, "POST", "/api/v1/review/runs", body, token); w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	var list api.AgentRunList
	if err := r.kube.List(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].Finalizers[0] != hold {
		t.Fatal("duplicate work or missing evidence hold")
	}
	if got := call(r, "POST", "/api/v1/review/runs", strings.Replace(body, "hello", "changed", 1), token).Code; got != 409 {
		t.Fatalf("changed original accepted: %d", got)
	}
	if got := call(r, "POST", "/api/v1/review/runs", strings.TrimSuffix(body, "}")+`,"namespace":"default"}`, token).Code; got != 400 {
		t.Fatalf("authority input accepted: %d", got)
	}
	// Simulate an archived API record. The immutable claim still excludes a
	// replacement create with the old request ID, even though no run exists.
	archived := &list.Items[0]
	archived.Finalizers = nil
	if err := r.kube.Update(context.Background(), archived); err != nil {
		t.Fatal(err)
	}
	if err := r.kube.Delete(context.Background(), archived); err != nil {
		t.Fatal(err)
	}
	if got := call(r, "POST", "/api/v1/review/runs", body, token).Code; got != 409 {
		t.Fatalf("archived request replayed: %d", got)
	}
}
func TestArchiveNeverUsesAbsenceAsCleanup(t *testing.T) {
	now := metav1.Now()
	run := &api.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "ux-test", Namespace: "celln-review-a-495", UID: "original", Labels: map[string]string{label: "495"}, Finalizers: []string{hold, "sympozium.ai/native-owner"}, DeletionTimestamp: &now}, Status: api.AgentRunStatus{Phase: api.AgentRunPhaseFailed}}
	r := fixture(t, run)
	path := "/api/v1/runs/ux-test/archive?namespace=celln-review-a-495"
	if got := call(r, "POST", path, `{"uid":"original"}`, strings.Repeat("x", 48)).Code; got != 409 {
		t.Fatalf("absence allowed archive: %d", got)
	}
	if got := call(r, "DELETE", "/api/v1/runs/ux-test?namespace=celln-review-a-495&uid=replacement", "", strings.Repeat("x", 48)).Code; got != 409 {
		t.Fatalf("replacement deleted: %d", got)
	}
}
