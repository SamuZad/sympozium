package cellnparent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// An owner the gateway no longer serves is established context loss: startup
// reports it instead of waiting, and cleanup treats the removal as teardown.
func TestOwnerRemovedFromPlaneIsReportedAsContextLossAndReleasesCleanup(t *testing.T) {
	run, binding := admissionFixture(t)
	var creates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/parents" {
			creates.Add(1)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":"original parent backend removed","retryAuthorized":false}`)
	}))
	defer server.Close()
	binding.Target = server.URL
	run.Status.CellnParent = &api.CellnParentStatus{Binding: binding, CreateAttempted: true}
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	store := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.AgentRun{}).WithObjects(run).Build()
	root := t.TempDir()
	token := filepath.Join(root, "token")
	if err := os.WriteFile(token, []byte("owner-removed-parent-credential-long"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(ApprovalConfig{APIVersion: "sympozium.ai/celln-parent-controller-v1", Approvals: []RunApproval{{Namespace: run.Namespace, Name: run.Name, Binding: binding, TokenFile: token}}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(run)
	observed, err := ReconcileStart(context.Background(), store, store, key, path)
	if err != nil || observed.Owner == nil || observed.Owner.Status != "ContextLost" || observed.Owner.Live || observed.Owner.Retry == nil || *observed.Owner.Retry || observed.Owner.Incarnation != binding.Incarnation {
		t.Fatalf("removed owner not reported as context loss: %+v %v", observed, err)
	}
	if creates.Load() != 0 {
		t.Fatal("removed owner triggered a replacement create")
	}
	if err := ReconcileStop(context.Background(), store, key, path); err != nil {
		t.Fatalf("cleanup after owner removal not released: %v", err)
	}
}
