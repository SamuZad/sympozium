package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/go-logr/logr"
	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/internal/cellnauthority"
	"github.com/sympozium-ai/sympozium/internal/cellnscoped"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const scopedOutcomeUnconfirmed = "Scoped execution outcome is uncertain. The controller will only read or clean up the original prepared owner; it will not create replacement authority. Ask an administrator to inspect the configured receiver and gateway."

var scopedReceiptPattern = regexp.MustCompile(`^(?:sha256:|blake3:)?[0-9a-f]{64}$`)

func scopedCatalogueSelected(run *api.AgentRun) bool {
	return run.Spec.CellnSelection != nil && (run.Spec.ExecutionLifecycle == "" || run.Spec.ExecutionLifecycle == "one-shot" || run.Spec.ExecutionLifecycle == "enduring")
}

// sharedCatalogueSelected disambiguates tool-free legacy and shared selections
// using only API reads. It intentionally runs before model normalization and
// prerequisite creation. A persisted scoped binding always wins recovery.
func (r *AgentRunReconciler) sharedCatalogueSelected(ctx context.Context, run *api.AgentRun) (bool, error) {
	if run.Status.CellnScoped != nil {
		return true, nil
	}
	selection := run.Spec.CellnSelection
	if selection == nil {
		return false, nil
	}
	// Explicit enduring catalogue intent is owned exclusively by the scoped
	// lifecycle wire. Unsupported wrappers are refused by its resolver; they may
	// never fall through to the older parent dispatcher.
	if run.Spec.ExecutionLifecycle == "enduring" {
		return true, nil
	}
	if len(selection.ClusterToolRefs) != 0 {
		return true, nil
	}
	if len(selection.ToolRefs) != 0 {
		return false, nil
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	var agent api.Agent
	if err := reader.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: run.Spec.AgentRef}, &agent); err != nil {
		return false, err
	}
	runtimeName := selection.RuntimeRef
	if runtimeName == "" {
		runtimeName = agent.Spec.RuntimeRef
	}
	if runtimeName == "" {
		return false, nil
	}
	var runtime api.AgentRuntime
	if err := reader.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: runtimeName}, &runtime); err != nil {
		return false, err
	}
	return runtime.Spec.CellnProfileRef != nil && runtime.Spec.Celln == nil, nil
}

func (r *AgentRunReconciler) scopedProgress(ctx context.Context, run *api.AgentRun, status metav1.ConditionStatus, reason, message string) error {
	return r.updateStatusWithRetry(ctx, run, func(current *api.AgentRun) {
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{Type: "CellnScopedExecution", Status: status, Reason: reason, Message: message, ObservedGeneration: current.Generation})
	})
}

func (r *AgentRunReconciler) reconcilePendingScoped(ctx context.Context, log logr.Logger, run *api.AgentRun) (ctrl.Result, error) {
	if run.Spec.Backend != "celln" || run.Spec.Celln != nil || !run.Spec.Task.IsString() || !scopedCatalogueSelected(run) {
		return ctrl.Result{}, r.failRun(ctx, run, "Scoped catalogue execution requires backend celln, a supported lifecycle, a string task, and no explicit artifacts")
	}
	if r.ScopedDispatcher == nil {
		if err := r.scopedProgress(ctx, run, metav1.ConditionFalse, "ScopedDispatchDisabled", "Operator scoped receiver configuration is absent; no legacy, model, OCI, or native execution was submitted"); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	prepared, err := r.ScopedDispatcher.Prepare(ctx, client.ObjectKeyFromObject(run))
	if err != nil {
		return r.scopedUncertain(ctx, run, "PreparationUnconfirmed", err)
	}
	final, err := r.ScopedDispatcher.EnsureFinal(ctx, prepared)
	if err != nil {
		return r.scopedUncertain(ctx, run, "FinalDecisionUnconfirmed", err)
	}
	if err := r.persistScopedBinding(ctx, run, prepared, final); err != nil {
		return ctrl.Result{}, err
	}
	if run.Status.CellnScoped == nil {
		return ctrl.Result{Requeue: true}, nil
	}
	// Reload after the status write so every following side effect is guarded by
	// the API-persisted immutable references, not this reconcile's local values.
	var current api.AgentRun
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(run), &current); err != nil {
		return ctrl.Result{}, err
	}
	*run = current
	prepared, final, err = r.loadScopedBinding(ctx, run)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !run.Status.CellnScoped.StartAttempted {
		if err := r.ScopedDispatcher.RevalidateAdmission(ctx, prepared); err != nil {
			return ctrl.Result{}, r.failRun(ctx, run, "Scoped authority changed before admission; no replacement execution was submitted")
		}
	}

	if run.Status.CellnScoped.ReceiverID == "" {
		enrolled, err := r.ScopedDispatcher.Enroll(ctx, prepared, final)
		if err != nil {
			return r.scopedUncertain(ctx, run, "ReceiverEnrollmentUnconfirmed", err)
		}
		if err := validateScopedOwner(enrolled.ID, enrolled.Owner); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.updateScopedStatus(ctx, run, func(s *api.CellnScopedStatus) error {
			if s.ReceiverID != "" && (s.ReceiverID != enrolled.ID || s.Owner != enrolled.Owner) {
				return errors.New("scoped receiver enrollment identity changed")
			}
			s.ReceiverID, s.Owner = enrolled.ID, enrolled.Owner
			return nil
		}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if run.Status.CellnScoped.StartAttempted && (run.Status.CellnScoped.GatewayRegistered || final.Decision.Route.Provider == "none") {
		observed, readErr := r.ScopedDispatcher.Read(ctx, run.Status.CellnScoped.ReceiverID, final)
		if readErr == nil {
			return r.applyScopedStatus(ctx, log, run, prepared, observed)
		}
		// Absence or loss of the receiver-owned admission journal is not proof
		// that the persisted attempt never arrived. Never relaunch from a JTI or
		// from a missing record; retain the finalizer and expose uncertainty.
		return r.scopedUncertain(ctx, run, "ExecutionOutcomeUnconfirmed", readErr)
	}
	decision, execution, model, err := r.ScopedDispatcher.StartTokens(final)
	if err != nil {
		return r.scopedUncertain(ctx, run, "OriginalAdmissionWindowUnavailable", err)
	}
	if decision.Route.Provider != "none" && !run.Status.CellnScoped.GatewayRegistered {
		if !run.Status.CellnScoped.GatewayRegistrationAttempted {
			if err := r.updateScopedStatus(ctx, run, func(s *api.CellnScopedStatus) error { s.GatewayRegistrationAttempted = true; return nil }); err != nil {
				return ctrl.Result{}, err
			}
			run.Status.CellnScoped.GatewayRegistrationAttempted = true
		}
		if err := r.ScopedDispatcher.RegisterGateway(ctx, final, decision, execution); err != nil {
			return r.scopedUncertain(ctx, run, "GatewayRegistrationUnconfirmed", err)
		}
		if err := r.updateScopedStatus(ctx, run, func(s *api.CellnScopedStatus) error { s.GatewayRegistered = true; return nil }); err != nil {
			return ctrl.Result{}, err
		}
		run.Status.CellnScoped.GatewayRegistered = true
	}
	if !run.Status.CellnScoped.StartAttempted {
		if err := r.updateScopedStatus(ctx, run, func(s *api.CellnScopedStatus) error { s.StartAttempted = true; return nil }); err != nil {
			return ctrl.Result{}, err
		}
		run.Status.CellnScoped.StartAttempted = true
	}
	observed, err := r.ScopedDispatcher.Start(ctx, run.Status.CellnScoped.ReceiverID, run.Status.CellnScoped.Owner, execution, model)
	if err != nil {
		return r.scopedUncertain(ctx, run, "ExecutionOutcomeUnconfirmed", err)
	}
	return r.applyScopedStatus(ctx, log, run, prepared, observed)
}

func (r *AgentRunReconciler) reconcileRunningScoped(ctx context.Context, log logr.Logger, run *api.AgentRun) (ctrl.Result, error) {
	prepared, final, err := r.loadScopedBinding(ctx, run)
	if err != nil {
		return ctrl.Result{}, err
	}
	observed, err := r.ScopedDispatcher.Read(ctx, run.Status.CellnScoped.ReceiverID, final)
	if err != nil {
		return r.scopedUncertain(ctx, run, "ExecutionOutcomeUnconfirmed", err)
	}
	return r.applyScopedStatus(ctx, log, run, prepared, observed)
}

func (r *AgentRunReconciler) scopedUncertain(ctx context.Context, run *api.AgentRun, reason string, cause error) (ctrl.Result, error) {
	if statusErr := r.scopedProgress(ctx, run, metav1.ConditionUnknown, reason, scopedOutcomeUnconfirmed); statusErr != nil {
		return ctrl.Result{}, statusErr
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, cause
}

func (r *AgentRunReconciler) persistScopedBinding(ctx context.Context, run *api.AgentRun, prepared *cellnauthority.StoredPreparation, final *cellnauthority.FinalizedPreparation) error {
	want := api.CellnScopedStatus{PreparationName: prepared.Name, PreparationUID: string(prepared.UID), DecisionName: final.Name, DecisionUID: string(final.UID)}
	if final.Decision.Parent != nil {
		want.ParentIncarnation = final.Decision.Parent.Incarnation
	}
	return r.updateStatusWithRetry(ctx, run, func(current *api.AgentRun) {
		if current.Status.CellnScoped == nil {
			current.Status.CellnScoped = &want
		}
	})
}

func (r *AgentRunReconciler) updateScopedStatus(ctx context.Context, run *api.AgentRun, mutate func(*api.CellnScopedStatus) error) error {
	var mutationErr error
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current api.AgentRun
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(run), &current); err != nil {
			return err
		}
		if current.UID != run.UID || current.Status.CellnScoped == nil {
			return errors.New("scoped run identity is unavailable")
		}
		before := *current.Status.CellnScoped
		if err := mutate(current.Status.CellnScoped); err != nil {
			mutationErr = err
			return nil
		}
		if before.PreparationName != current.Status.CellnScoped.PreparationName || before.PreparationUID != current.Status.CellnScoped.PreparationUID || before.DecisionName != current.Status.CellnScoped.DecisionName || before.DecisionUID != current.Status.CellnScoped.DecisionUID || before.ParentIncarnation != current.Status.CellnScoped.ParentIncarnation || before.TurnID != current.Status.CellnScoped.TurnID || (before.ReceiverID != "" && (before.ReceiverID != current.Status.CellnScoped.ReceiverID || before.Owner != current.Status.CellnScoped.Owner)) {
			return errors.New("scoped immutable status identity changed")
		}
		return r.Status().Update(ctx, &current)
	})
	if mutationErr != nil {
		return mutationErr
	}
	return err
}

func (r *AgentRunReconciler) loadScopedBinding(ctx context.Context, run *api.AgentRun) (*cellnauthority.StoredPreparation, *cellnauthority.FinalizedPreparation, error) {
	if r.ScopedDispatcher == nil || r.APIReader == nil || run.Status.CellnScoped == nil {
		return nil, nil, errors.New("scoped dispatcher recovery is unavailable")
	}
	s := run.Status.CellnScoped
	prepared, err := r.ScopedDispatcher.Store.Load(ctx, s.PreparationName)
	if err != nil {
		return nil, nil, err
	}
	if prepared.Operation.Resolution.Execution.Source.ClusterID != r.ScopedDispatcher.ClusterID {
		return nil, nil, errors.New("scoped controller cluster identity changed")
	}
	if err := r.ScopedDispatcher.Store.ValidateCurrent(ctx, client.ObjectKeyFromObject(run), prepared); err != nil {
		return nil, nil, err
	}
	if string(prepared.UID) != s.PreparationUID {
		return nil, nil, errors.New("protected scoped preparation UID changed")
	}
	final, err := r.ScopedDispatcher.Store.LoadFinal(ctx, s.DecisionName, prepared)
	if err != nil {
		return nil, nil, err
	}
	if string(final.UID) != s.DecisionUID {
		return nil, nil, errors.New("protected scoped decision UID changed")
	}
	if final.Decision.Parent == nil {
		if s.ParentIncarnation != "" || s.TurnID != "" {
			return nil, nil, errors.New("one-shot scoped status carries parent identity")
		}
	} else if s.ParentIncarnation != final.Decision.Parent.Incarnation || (final.Decision.Parent.TurnID != nil && s.TurnID != *final.Decision.Parent.TurnID) {
		return nil, nil, errors.New("scoped parent identity changed")
	}
	return prepared, final, nil
}

func validateScopedOwner(id, owner string) error {
	if id == "" || owner == "" || len(id) > 256 || len(owner) > 256 {
		return errors.New("scoped receiver returned an invalid owner identity")
	}
	return nil
}

func (r *AgentRunReconciler) applyScopedStatus(ctx context.Context, log logr.Logger, run *api.AgentRun, prepared *cellnauthority.StoredPreparation, observed cellnscoped.OperationStatus) (ctrl.Result, error) {
	s := run.Status.CellnScoped
	if s == nil || observed.ID != s.ReceiverID || observed.Owner != s.Owner || validateScopedOwner(observed.ID, observed.Owner) != nil {
		return ctrl.Result{}, errors.New("scoped receiver result owner mismatch")
	}
	if int64(len(observed.Output)) > prepared.Operation.Resolution.Execution.RuntimeLimits.OutputBytes || len(observed.Reason) > 4096 || (observed.ReceiptDigest != "" && !scopedReceiptPattern.MatchString(observed.ReceiptDigest)) {
		return ctrl.Result{}, errors.New("scoped receiver result violates the prepared output contract")
	}
	if err := validateScopedCorrelation(prepared, observed); err != nil {
		return ctrl.Result{}, err
	}
	executionProvenance, err := boundedNativeEvidence(observed.Execution)
	if err != nil {
		return ctrl.Result{}, err
	}
	substrateProvenance, err := boundedNativeEvidence(observed.Substrate)
	if err != nil {
		return ctrl.Result{}, err
	}
	active := []string{"Prepared", "Admitting", "Admitted", "Running", "Cancelling", "Uncertain"}
	terminal := []string{"Succeeded", "Failed", "Refused", "Cancelled"}
	if !slices.Contains(active, observed.Phase) && !slices.Contains(terminal, observed.Phase) {
		return ctrl.Result{}, errors.New("scoped receiver returned an unknown phase")
	}
	now := metav1.Now()
	if err := r.updateScopedStatus(ctx, run, func(state *api.CellnScopedStatus) error {
		state.NativePhase = observed.Phase
		if observed.ReceiptDigest != "" {
			if state.ReceiptDigest != "" && state.ReceiptDigest != observed.ReceiptDigest {
				return errors.New("scoped receipt digest changed")
			}
			state.ReceiptDigest = observed.ReceiptDigest
		}
		if observed.Output != "" {
			if state.Output != "" && state.Output != observed.Output {
				return errors.New("scoped correlated output changed")
			}
			state.Output = observed.Output
		}
		for current, next := range map[*string]string{&state.ParentID: observed.ParentID, &state.ChildID: observed.ChildID, &state.CellID: observed.CellID, &state.ExecutionProvenance: executionProvenance, &state.SubstrateProvenance: substrateProvenance} {
			if next != "" {
				if *current != "" && *current != next {
					return errors.New("scoped native provenance changed")
				}
				*current = next
			}
		}
		return nil
	}); err != nil {
		return ctrl.Result{}, err
	}
	if observed.Phase == "Uncertain" {
		return r.scopedUncertain(ctx, run, "NativeOwnerUncertain", errors.New("original native owner context is unavailable; no replacement execution is permitted"))
	}
	if slices.Contains(active, observed.Phase) {
		if err := r.updateStatusWithRetry(ctx, run, func(current *api.AgentRun) {
			current.Status.Phase = api.AgentRunPhaseRunning
			if current.Status.StartedAt == nil {
				current.Status.StartedAt = &now
			}
			reason := "OwnerRecordObserved"
			message := "The configured receiver returned the original prepared owner record"
			if current.Spec.ExecutionLifecycle == "enduring" && observed.Phase == "Running" && current.Status.CellnScoped != nil && current.Status.CellnScoped.ReceiptDigest != "" && current.Status.CellnScoped.Output != "" {
				reason = "EnduringParentReady"
				message = "The original native parent completed its initial turn and remains running"
				current.Status.Result = current.Status.CellnScoped.Output
			}
			meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{Type: "CellnScopedExecution", Status: metav1.ConditionTrue, Reason: reason, Message: message, ObservedGeneration: current.Generation})
		}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if observed.Phase == "Succeeded" && observed.ReceiptDigest == "" {
		return ctrl.Result{}, errors.New("scoped success lacks a receipt digest")
	}
	if err := r.updateStatusWithRetry(ctx, run, func(current *api.AgentRun) {
		current.Status.CompletedAt = &now
		if observed.Phase == "Succeeded" {
			current.Status.Phase, current.Status.Result, current.Status.Error = api.AgentRunPhaseSucceeded, observed.Output, ""
		} else {
			// Receiver details may contain operator topology or guest-controlled
			// diagnostics. Persist only the bounded phase in tenant-visible status.
			current.Status.Phase, current.Status.Error = api.AgentRunPhaseFailed, fmt.Sprintf("Celln scoped execution %s", observed.Phase)
		}
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{Type: "CellnScopedExecution", Status: metav1.ConditionTrue, Reason: "TerminalOwnerRecordObserved", Message: "The original prepared owner returned a terminal record; cleanup confirmation is pending", ObservedGeneration: current.Generation})
	}); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("Scoped Celln execution reached terminal owner record", "phase", observed.Phase)
	return ctrl.Result{Requeue: true}, nil
}

func validateScopedCorrelation(prepared *cellnauthority.StoredPreparation, observed cellnscoped.OperationStatus) error {
	d := prepared.Operation.Resolution.Decision
	if d.Parent == nil {
		if observed.ParentIncarnation != "" || observed.TurnID != "" {
			return errors.New("one-shot receiver result carries parent correlation")
		}
		return nil
	}
	if observed.ParentIncarnation != d.Parent.Incarnation {
		return errors.New("scoped receiver returned another parent incarnation")
	}
	if d.Parent.TurnID != nil && observed.TurnID != *d.Parent.TurnID {
		return errors.New("scoped receiver returned another turn identity")
	}
	return nil
}

func boundedNativeEvidence(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	if len(raw) > 65536 || !json.Valid(raw) {
		return "", errors.New("scoped native provenance is invalid or exceeds its bound")
	}
	var compact []byte
	buffer := &bytes.Buffer{}
	if err := json.Compact(buffer, raw); err != nil {
		return "", err
	}
	compact = buffer.Bytes()
	return string(compact), nil
}

func (r *AgentRunReconciler) cleanupScoped(ctx context.Context, run *api.AgentRun) (bool, error) {
	if r.ScopedDispatcher == nil || run.Status.CellnScoped == nil {
		return false, errors.New("scoped cleanup configuration is unavailable")
	}
	if run.Status.CellnScoped.CleanupConfirmed {
		return true, nil
	}
	prepared, final, err := r.loadScopedBinding(ctx, run)
	if err != nil {
		return false, err
	}
	if run.Status.CellnScoped.ReceiverID == "" {
		enrolled, err := r.ScopedDispatcher.Enroll(ctx, prepared, final)
		if err != nil {
			return false, err
		}
		if err := validateScopedOwner(enrolled.ID, enrolled.Owner); err != nil {
			return false, err
		}
		if err := r.updateScopedStatus(ctx, run, func(s *api.CellnScopedStatus) error { s.ReceiverID, s.Owner = enrolled.ID, enrolled.Owner; return nil }); err != nil {
			return false, err
		}
		return false, nil
	}
	status, err := r.ScopedDispatcher.Cleanup(ctx, run.Status.CellnScoped.ReceiverID, final, run.Status.CellnScoped.GatewayRegistrationAttempted)
	if err != nil {
		return false, err
	}
	if status.ID != run.Status.CellnScoped.ReceiverID || status.Owner != run.Status.CellnScoped.Owner || !status.CleanupConfirmed {
		return false, errors.New("scoped receiver has not confirmed cleanup for the prepared owner")
	}
	if err := r.updateScopedStatus(ctx, run, func(s *api.CellnScopedStatus) error {
		s.CleanupConfirmed = true
		s.NativePhase = status.Phase
		return nil
	}); err != nil {
		return false, err
	}
	return true, nil
}
