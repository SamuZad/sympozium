package controller

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/internal/cellnauthority"
	"github.com/sympozium-ai/sympozium/internal/cellnparent"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ParentAdmission interface {
	Admit(context.Context, types.NamespacedName) error
	// SupportsPlatform reports whether wrapper-shaped selections (platform
	// runtime profile, cluster tools) are admitted here rather than held for
	// the scoped receiver.
	SupportsPlatform() bool
}

func (r *AgentRunReconciler) parentReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// Native parent readiness is not task success. Keep the enduring run live for
// turn delivery; never create an OCI workload or mark the initial task complete.
func (r *AgentRunReconciler) reconcileCellnParent(ctx context.Context, run *api.AgentRun) (ctrl.Result, error) {
	if r.ParentConfigPath == "" {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, fmt.Errorf("parent operator configuration unavailable; preserve owner")
	}
	if r.ParentAdmission != nil && run.Status.CellnParent == nil {
		if err := r.ParentAdmission.Admit(ctx, client.ObjectKeyFromObject(run)); err != nil {
			if statusErr := r.recordParentAdmissionPending(ctx, run, err); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, fmt.Errorf("parent admission unavailable; no execution attempted: %w", err)
		}
	}
	observed, observeErr := cellnparent.ReconcileStart(ctx, r.Client, r.parentReader(), client.ObjectKeyFromObject(run), r.ParentConfigPath)
	var turnDone bool
	var turnErr error
	turnChecked := false
	if observeErr == nil && observed.Owner != nil && observed.Owner.Live && observed.Owner.Status == "Ready" {
		turnChecked = true
		turnDone, turnErr = cellnparent.ReconcileInitialTurn(ctx, r.Client, r.parentReader(), client.ObjectKeyFromObject(run), r.ParentConfigPath)
	} else if observeErr == nil && observed.Owner != nil && (observed.Owner.Status == "ContextLost" || observed.Owner.Status == "Stopped" || observed.Owner.Status == "TeardownUncertain") {
		turnChecked = true
		turnDone, turnErr = cellnparent.RecoverInitialTurn(ctx, r.Client, r.parentReader(), client.ObjectKeyFromObject(run), r.ParentConfigPath)
	}
	// Startup may have updated status; never overwrite that durable claim with
	// the stale object passed into this reconciliation.
	var fresh api.AgentRun
	if err := r.parentReader().Get(ctx, client.ObjectKeyFromObject(run), &fresh); err != nil {
		return ctrl.Result{}, err
	}
	if fresh.UID != run.UID || fresh.DeletionTimestamp != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	if fresh.Status.Phase != "" && fresh.Status.Phase != api.AgentRunPhasePending && fresh.Status.Phase != api.AgentRunPhaseRunning {
		return ctrl.Result{Requeue: true}, nil
	}
	before := fresh.DeepCopy()
	if turnChecked {
		turnCondition := metav1.Condition{Type: "CellnInitialTurnComplete", Status: metav1.ConditionFalse, Reason: "Pending", Message: "Initial turn pending; preserving its original identity", ObservedGeneration: fresh.Generation}
		if turnDone {
			turnCondition.Status = metav1.ConditionTrue
			turnCondition.Reason = "Committed"
			turnCondition.Message = "Initial turn result recorded; parent lifecycle is reported separately"
		}
		if turnErr != nil {
			turnCondition.Reason = "ReconciliationRequired"
			turnCondition.Message = "Initial turn outcome unavailable; no automatic resubmission"
		}
		meta.SetStatusCondition(&fresh.Status.Conditions, turnCondition)
	}
	condition := metav1.Condition{Type: "CellnParentReady", Status: metav1.ConditionFalse, Reason: "Initializing", Message: "Parent startup pending; no turn completion implied", ObservedGeneration: fresh.Generation}
	if observeErr != nil {
		condition.Reason = "ReconciliationRequired"
		condition.Message = "Parent outcome unavailable; preserving original incarnation without replay"
	} else if observed.Owner != nil {
		switch observed.Owner.Status {
		case "Ready", "TurnActive":
			fresh.Status.Phase = api.AgentRunPhaseRunning
			condition.Status = metav1.ConditionTrue
			condition.Reason = observed.Owner.Status
			condition.Message = "Native parent initialized; turn completion is tracked separately"
		case "ContextLost", "Stopped", "TeardownUncertain":
			outcome := recordOwnerOutcome(&fresh, observed.Owner.Status)
			meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{Type: "CellnParentReady", Status: metav1.ConditionFalse, Reason: observed.Owner.Status, Message: outcome, ObservedGeneration: fresh.Generation})
			slog.WarnContext(ctx, "celln.parent.owner-lost", "agent_run", fresh.Name, "ownerStatus", observed.Owner.Status, "detail", outcome)
			// failRun rereads status and applies only terminal fields. Persist
			// these independent turn/owner observations before that fresh read.
			if err := r.Status().Update(ctx, &fresh); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, r.failRun(ctx, &fresh, "Celln parent context lost or stopped; reconcile recorded turns: "+outcome)
		}
	}
	meta.SetStatusCondition(&fresh.Status.Conditions, condition)
	if apiequality.Semantic.DeepEqual(before.Status, fresh.Status) {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	if err := r.Status().Update(ctx, &fresh); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
}

// Report admission without leaking operator paths or overwriting a concurrently
// bound parent. This observation grants no authority and does not start work.
func (r *AgentRunReconciler) recordParentAdmissionPending(ctx context.Context, observed *api.AgentRun, cause error) error {
	var fresh api.AgentRun
	if err := r.parentReader().Get(ctx, client.ObjectKeyFromObject(observed), &fresh); err != nil {
		return err
	}
	if fresh.UID != observed.UID || fresh.Generation != observed.Generation || fresh.DeletionTimestamp != nil || fresh.Status.CellnParent != nil || (fresh.Status.Phase != "" && fresh.Status.Phase != api.AgentRunPhasePending) {
		return nil
	}
	before := fresh.DeepCopy()
	message := "Waiting for a matching operator-prepared parent registration and current grants. Ask the operator to check admission; do not create a replacement run."
	// A platform refusal has a stable, secret-free reason code worth showing;
	// anything else stays generic so operator paths never reach tenant status.
	if reason := cellnauthority.PlatformReason(cause); reason != "" {
		message = "Platform policy refused admission (" + reason + "). Ask the operator to authorise this namespace, runtime profile, tools and model route; do not create a replacement run."
	}
	meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{Type: "CellnParentReady", Status: metav1.ConditionFalse, Reason: "AdmissionPending", Message: message, ObservedGeneration: fresh.Generation})
	if apiequality.Semantic.DeepEqual(before.Status, fresh.Status) {
		return nil
	}
	return r.Status().Update(ctx, &fresh)
}

// recordOwnerOutcome freezes the first terminal owner observation on the run
// status and returns the human-readable failure signature. The broker reports
// only a bare status, so the signature is built from what the controller saw:
// whether Ready was ever live, how long after admission the loss landed, and
// the frozen incarnation and launch-profile hashes that identify the worker
// the owner failed to prepare. Recording grants no replay authority.
func recordOwnerOutcome(run *api.AgentRun, status string) string {
	reachedReady := meta.IsStatusConditionTrue(run.Status.Conditions, "CellnParentReady")
	now := metav1.Now()
	if run.Status.CellnParent == nil {
		return fmt.Sprintf("owner=%s reachedReady=%t; parent context unavailable; no automatic reconstruction", status, reachedReady)
	}
	if run.Status.CellnParent.OwnerOutcome == nil {
		run.Status.CellnParent.OwnerOutcome = &api.CellnParentOwnerOutcome{Status: status, ReachedReady: reachedReady, ObservedAt: now}
	}
	outcome := run.Status.CellnParent.OwnerOutcome
	age := "unknown"
	if run.Status.CellnParent.AdmittedAt != nil {
		age = now.Sub(run.Status.CellnParent.AdmittedAt.Time).Truncate(time.Second).String()
	}
	return fmt.Sprintf("owner=%s reachedReady=%t admittedAge=%s incarnation=%s launchProfile=%s; parent context unavailable; no automatic reconstruction",
		outcome.Status, outcome.ReachedReady, age, run.Status.CellnParent.Binding.Incarnation, run.Status.CellnParent.Binding.LaunchProfile)
}

func (r *AgentRunReconciler) stopCellnParent(ctx context.Context, run *api.AgentRun) error {
	if run.Status.CellnParent == nil || !run.Status.CellnParent.CreateAttempted {
		return nil
	}
	if r.ParentConfigPath == "" {
		return fmt.Errorf("parent cleanup requires original operator configuration")
	}
	return cellnparent.ReconcileStop(ctx, r.parentReader(), client.ObjectKeyFromObject(run), r.ParentConfigPath)
}
