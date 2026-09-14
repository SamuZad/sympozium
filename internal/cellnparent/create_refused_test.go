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
	"time"

	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func refusalFixture(t *testing.T, handler http.HandlerFunc, status *api.CellnParentStatus) (client.Client, client.ObjectKey, string, func()) {
	t.Helper()
	run, binding := admissionFixture(t)
	server := httptest.NewServer(handler)
	binding.Target = server.URL
	if status != nil {
		status.Binding = binding
		run.Status.CellnParent = status
	}
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	store := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.AgentRun{}).WithObjects(run).Build()
	root := t.TempDir()
	token := filepath.Join(root, "token")
	if err := os.WriteFile(token, []byte("create-refused-parent-credential-long"), 0600); err != nil {
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
	return store, client.ObjectKeyFromObject(run), path, server.Close
}

// A create the owner refuses is terminal: startup reports CreateRefused once,
// never re-POSTs, and cleanup releases without contacting the owner.
func TestOwnerRefusedCreateIsTerminalAndDeletable(t *testing.T) {
	var creates, stops atomic.Int32
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/parents":
			creates.Add(1)
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error":"parent creation refused; reconcile incarnation","retryAuthorized":false}`)
		case r.Method == http.MethodPost:
			stops.Add(1)
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"parent not found"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
	store, key, path, stop := refusalFixture(t, handler, &api.CellnParentStatus{})
	defer stop()
	ctx := context.Background()
	observed, err := ReconcileStart(ctx, store, store, key, path)
	if err != nil || observed.Owner == nil || observed.Owner.Status != OwnerCreateRefused || observed.Owner.Retry == nil || *observed.Owner.Retry {
		t.Fatalf("owner refusal not reported as terminal: %+v %v", observed, err)
	}
	if creates.Load() != 1 {
		t.Fatalf("creates = %d, want exactly one", creates.Load())
	}
	var run api.AgentRun
	if err := store.Get(ctx, key, &run); err != nil {
		t.Fatal(err)
	}
	run.Status.CellnParent.OwnerOutcome = &api.CellnParentOwnerOutcome{Status: OwnerCreateRefused, ObservedAt: metav1.Now()}
	if err := store.Status().Update(ctx, &run); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileStop(ctx, store, key, path); err != nil {
		t.Fatalf("refused run cleanup not released: %v", err)
	}
	if stops.Load() != 0 || creates.Load() != 1 {
		t.Fatalf("refused run cleanup contacted the owner (stops=%d creates=%d)", stops.Load(), creates.Load())
	}
}

// An owner that holds nothing for a settled incarnation (the refusal reply was
// lost, or the owner restarted) is context loss and releases cleanup; within
// the grace window the same answer stays uncertain.
func TestOwnerHoldingNothingIsSettledOnlyAfterGrace(t *testing.T) {
	notFound := func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/parents" {
			t.Error("a missing parent triggered a replacement create")
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"parent owner unknown","retryAuthorized":false}`)
	}
	ctx := context.Background()
	recent := metav1.NewTime(time.Now().Add(-10 * time.Second))
	store, key, path, stop := refusalFixture(t, notFound, &api.CellnParentStatus{CreateAttempted: true, AdmittedAt: &recent})
	if _, err := ReconcileStart(ctx, store, store, key, path); err == nil {
		t.Fatal("a fresh create with no owner record was treated as settled")
	}
	if err := ReconcileStop(ctx, store, key, path); err == nil {
		t.Fatal("cleanup released while the create could still be in flight")
	}
	stop()
	old := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	store, key, path, stop = refusalFixture(t, notFound, &api.CellnParentStatus{CreateAttempted: true, AdmittedAt: &old})
	defer stop()
	observed, err := ReconcileStart(ctx, store, store, key, path)
	if err != nil || observed.Owner == nil || observed.Owner.Status != "ContextLost" || observed.Owner.Live {
		t.Fatalf("settled missing parent not reported as context loss: %+v %v", observed, err)
	}
	if err := ReconcileStop(ctx, store, key, path); err != nil {
		t.Fatalf("settled missing parent did not release cleanup: %v", err)
	}
	var run api.AgentRun
	_ = store.Get(ctx, key, &run)
	run.Status.CellnParent.AdmittedAt = nil
	if CreateSettled(&run, time.Now()) {
		t.Fatal("a run without an admission time was treated as settled")
	}
}
