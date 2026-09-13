package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestSharedCatalogueDisabledStopsBeforeLegacyModelAndOCIPaths(t *testing.T) {
	run := &api.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "tenant", UID: types.UID("shared-run-uid"), Generation: 1, Finalizers: []string{agentRunFinalizer}},
		Spec:       api.AgentRunSpec{AgentRef: "missing-agent-must-not-be-read", Backend: "celln", Task: api.NewStringTask("bounded task"), Model: api.ModelSpec{ModelRef: "missing-model-must-not-be-resolved"}, CellnSelection: &api.CellnCatalogueSelection{ClusterToolRefs: []api.ClusterCellnToolRef{{Name: "shared-tool", Revision: "v1"}}}},
		Status:     api.AgentRunStatus{CellnOnly: true},
	}
	r := newAgentRunTestReconciler(t, run)
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("disabled scoped selection was not held for explicit operator configuration")
	}
	var current api.AgentRun
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(run), &current); err != nil {
		t.Fatal(err)
	}
	condition := meta.FindStatusCondition(current.Status.Conditions, "CellnScopedExecution")
	if condition == nil || condition.Reason != "ScopedDispatchDisabled" || current.Status.JobName != "" || current.Status.CellnActionID != "" || current.Status.CellnIssuance != nil {
		t.Fatalf("shared selection escaped fail-closed boundary: status=%#v", current.Status)
	}
	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace(run.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatal("disabled scoped selection created a legacy OCI Job")
	}
}

func TestLegacyNamespacedCatalogueSelectionIsNotMisrepresentedAsScoped(t *testing.T) {
	run := &api.AgentRun{Spec: api.AgentRunSpec{CellnSelection: &api.CellnCatalogueSelection{ToolRefs: []api.CellnCatalogueToolRef{{Name: "legacy", Revision: "v1"}}}}}
	r := &AgentRunReconciler{}
	shared, err := r.sharedCatalogueSelected(context.Background(), run)
	if err != nil || shared {
		t.Fatalf("legacy selection classified as scoped: shared=%v err=%v", shared, err)
	}
}

func TestScopedTerminalRetainsFinalizerWhenCleanupIsUnconfirmed(t *testing.T) {
	run := &api.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "terminal", Namespace: "tenant", UID: types.UID("terminal-uid"), Finalizers: []string{agentRunFinalizer}},
		Spec:       api.AgentRunSpec{Backend: "celln", CellnSelection: &api.CellnCatalogueSelection{}},
		Status:     api.AgentRunStatus{Phase: api.AgentRunPhaseFailed, CellnScoped: &api.CellnScopedStatus{PreparationName: "celln-op-" + strings.Repeat("a", 64), PreparationUID: "p", DecisionName: "celln-final-" + strings.Repeat("b", 64), DecisionUID: "d"}},
	}
	r := newAgentRunTestReconciler(t, run)
	result, err := r.reconcileCompleted(context.Background(), logr.Discard(), run)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("unconfirmed cleanup did not requeue")
	}
	var current api.AgentRun
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(run), &current); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(&current, agentRunFinalizer) {
		t.Fatal("finalizer removed without real scoped cleanup confirmation")
	}
}
