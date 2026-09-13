package controller

import (
	"context"
	"fmt"
	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// finishScopedOnly runs after authenticated scoped cleanup. The dedicated
// controller neither owns legacy resources nor has permission to inspect them.
// An unrelated observer finalizer must not send it into legacy cleanup, history
// pruning or successor dispatch. Only this controller's own finalizer is removed.
func (r *AgentRunReconciler) finishScopedOnly(ctx context.Context, run *api.AgentRun) (ctrl.Result, error) {
	if run.Status.JobName != "" || run.Status.PostRunJobName != "" || run.Status.DeploymentName != "" || run.Status.ServiceName != "" || run.Status.CellnActionID != "" || run.Status.CellnParent != nil {
		return ctrl.Result{}, fmt.Errorf("scoped-only controller cannot finalize a legacy execution binding")
	}
	// Shared execution persists CellnScoped before receiver enrollment. A
	// deletion with no binding is cancelled before admission, not VM teardown.
	// Status.Update's resourceVersion prevents racing a newly saved binding.
	condition := meta.FindStatusCondition(run.Status.Conditions, "CellnScopedExecution")
	if run.DeletionTimestamp != nil && run.Status.CellnScoped == nil && (condition == nil || condition.Reason != "CancelledBeforeAdmission" && condition.Reason != "AdmissionRefused") {
		now := metav1.Now()
		run.Status.Phase = api.AgentRunPhaseFailed
		run.Status.CompletedAt = &now
		run.Status.Error = "Cancelled before admission; no native execution started"
		meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{Type: "CellnScopedExecution", Status: metav1.ConditionFalse, Reason: "CancelledBeforeAdmission", Message: run.Status.Error, ObservedGeneration: run.Generation})
		if err := r.Status().Update(ctx, run); err != nil {
			return ctrl.Result{}, err
		}
	}
	if !controllerutil.ContainsFinalizer(run, agentRunFinalizer) {
		return ctrl.Result{}, nil
	}
	patch := client.MergeFromWithOptions(run.DeepCopy(), client.MergeFromWithOptimisticLock{})
	controllerutil.RemoveFinalizer(run, agentRunFinalizer)
	return ctrl.Result{}, r.Patch(ctx, run, patch)
}
