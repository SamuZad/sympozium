package cellnparent

import (
	"context"
	"errors"
	"fmt"
	"time"

	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var ErrTurnBusy = errors.New("parent already owns a different turn; wait for its reconciliation")

// ErrParentLeaseExpired marks refusals of new turn claims after the original
// execution lease elapsed. It never fires for the already-owning turn's
// idempotent re-claim. Callers use errors.Is to surface an actionable reason.
var ErrParentLeaseExpired = errors.New("parent lease expired")

func readTurnPair(ctx context.Context, reader client.Reader, key types.NamespacedName) (*api.AgentRun, *api.AgentRunTurn, error) {
	var turn api.AgentRunTurn
	if err := reader.Get(ctx, key, &turn); err != nil {
		return nil, nil, err
	}
	var run api.AgentRun
	if err := reader.Get(ctx, types.NamespacedName{Namespace: turn.Namespace, Name: turn.Spec.RunName}, &run); err != nil {
		return nil, nil, err
	}
	if _, err := BindTurn(&run, &turn); err != nil {
		return nil, nil, err
	}
	return &run, &turn, nil
}

// ParentLeaseExpired reports whether the original execution lease recorded at
// parent admission has elapsed. A missing admission stamp predates lease
// enforcement and is not treated as expiry. The deadline is inclusive: the
// lease covers [admittedAt, admittedAt+leaseSeconds), so work starting exactly
// at the deadline is already outside the original authority.
func ParentLeaseExpired(parent *api.CellnParentStatus, leaseSeconds int64, now time.Time) bool {
	if parent == nil || parent.AdmittedAt == nil || parent.AdmittedAt.IsZero() || leaseSeconds <= 0 {
		return false
	}
	return !now.Before(parent.AdmittedAt.Add(time.Duration(leaseSeconds) * time.Second))
}

// ClaimTurnSlot serializes subsequent turns with one parent status CAS. It
// admits no HTTP request. The initial turn must already have committed; budgets
// are spent once when the slot is assigned, never refunded after uncertainty.
// An expired original lease refuses new claims, but the already-owning turn
// keeps its idempotent re-claim so in-flight reconciliation can finish.
func ClaimTurnSlot(ctx context.Context, writer client.Client, reader client.Reader, key types.NamespacedName, config string, now time.Time) error {
	run, turn, err := readTurnPair(ctx, reader, key)
	if err != nil {
		return err
	}
	_, transport, err := LoadApproval(config, run)
	if err != nil {
		return err
	}
	transport.Close()
	parent := run.Status.CellnParent
	if run.Status.Phase != api.AgentRunPhaseRunning || parent.InitialTurn == nil || parent.InitialTurn.Result == nil || !parent.InitialTurn.Result.Succeeded {
		return fmt.Errorf("subsequent turn requires a running parent and committed initial turn")
	}
	if turn.Status.Execution != nil && turn.Status.Execution.Result != nil {
		return fmt.Errorf("completed turn cannot acquire another slot")
	}
	if active := parent.ActiveTurn; active != nil {
		if active.Name == turn.Name && active.UID == string(turn.UID) {
			return nil
		}
		return ErrTurnBusy
	}
	if ParentLeaseExpired(parent, int64(run.Spec.Enduring.LeaseSeconds), now) {
		return fmt.Errorf("%w; original admission does not authorize new turns", ErrParentLeaseExpired)
	}
	if parent.AcceptedTurns < 0 || parent.AcceptedTurns >= run.Spec.Enduring.MaxTurns-1 {
		return fmt.Errorf("parent turn budget exhausted")
	}
	parent.ActiveTurn = &api.CellnActiveTurn{Name: turn.Name, UID: string(turn.UID)}
	parent.AcceptedTurns++
	return writer.Status().Update(ctx, run)
}

// ReleaseTurnSlot requires the exact child's durable completed record, not just
// a received HTTP response. Deleted/missing/uncertain records retain the slot.
// A late acknowledgement can never release another turn's ownership.
func ReleaseTurnSlot(ctx context.Context, writer client.Client, reader client.Reader, key types.NamespacedName) error {
	run, turn, err := readTurnPair(ctx, reader, key)
	if err != nil {
		return err
	}
	execution := turn.Status.Execution
	if execution == nil || !execution.Attempted || execution.Result == nil {
		return fmt.Errorf("turn completion is not durably recorded")
	}
	active := run.Status.CellnParent.ActiveTurn
	if active == nil {
		return nil
	}
	if active.Name != turn.Name || active.UID != string(turn.UID) {
		return ErrTurnBusy
	}
	run.Status.CellnParent.ActiveTurn = nil
	return writer.Status().Update(ctx, run)
}
