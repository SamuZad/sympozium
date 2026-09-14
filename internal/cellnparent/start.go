package cellnparent

import (
	"context"
	"errors"
	"time"

	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// OwnerCreateRefused is the owner status recorded when the owner refused the
// single create for an incarnation.
const OwnerCreateRefused = "CreateRefused"

// createSettleGrace bounds how long a create can still be in flight after it
// was first attempted. Owners answer creates synchronously within seconds; past
// this, an owner that holds nothing for the incarnation never will.
const createSettleGrace = 2 * time.Minute

// CreateSettled reports whether the run's single create attempt is old enough
// that an owner holding nothing for its incarnation is a fact, not a race.
func CreateSettled(run *api.AgentRun, now time.Time) bool {
	p := run.Status.CellnParent
	return p != nil && p.CreateAttempted && p.AdmittedAt != nil && now.Sub(p.AdmittedAt.Time) > createSettleGrace
}

// StartObservation describes startup only, not turn execution or completion.
type StartObservation struct {
	Prepared         bool
	CreationAccepted bool
	Owner            *Status
}

// ReconcileStart performs one durable startup step. First reconciliation saves
// approval without dispatch; the next claims the attempt before POST. Subsequent
// reconciliations only GET the frozen incarnation, including after a lost reply.
// A missing owner after an attempted create is uncertainty, never retry authority.
func ReconcileStart(ctx context.Context, writer client.Client, reader client.Reader, key types.NamespacedName, configPath string) (StartObservation, error) {
	var run api.AgentRun
	if err := reader.Get(ctx, key, &run); err != nil {
		return StartObservation{}, err
	}
	approval, transport, err := LoadApproval(configPath, &run)
	if err != nil {
		return StartObservation{}, err
	}
	defer transport.Close()
	if run.Status.CellnParent == nil {
		if err := Prepare(ctx, writer, reader, key, approval); err != nil {
			return StartObservation{}, err
		}
		return StartObservation{Prepared: true}, nil
	}
	claimed, err := ClaimCreate(ctx, writer, reader, key, approval)
	if err != nil {
		return StartObservation{}, err
	}
	retry := false
	if claimed {
		if err := transport.Create(ctx, approval.LaunchProfile, approval.Incarnation); err != nil {
			if errors.Is(err, ErrCreateRefused) {
				// The owner refused this create (capacity or authority) and
				// started nothing. The run ends; the incarnation is never retried.
				return StartObservation{Prepared: true, Owner: &Status{Incarnation: approval.Incarnation, Status: OwnerCreateRefused, Retry: &retry}}, nil
			}
			return StartObservation{}, err
		}
		return StartObservation{Prepared: true, CreationAccepted: true}, nil
	}
	owner, err := transport.Status(ctx, approval.Incarnation)
	if errors.Is(err, ErrOwnerRemoved) {
		// The gateway no longer serves the owner that held this parent (its
		// node left the fleet or its process was replaced). That is established
		// context loss, not uncertainty: report it instead of waiting forever.
		return StartObservation{Prepared: true, Owner: &Status{Incarnation: approval.Incarnation, Status: "ContextLost", Retry: &retry}}, nil
	}
	if errors.Is(err, ErrNotFound) && CreateSettled(&run, time.Now()) {
		// The gateway routed to the bound owner, which holds nothing for this
		// incarnation long after the create: it was refused (and the reply
		// lost) or the owner process restarted. Either way no parent exists.
		return StartObservation{Prepared: true, Owner: &Status{Incarnation: approval.Incarnation, Status: "ContextLost", Retry: &retry}}, nil
	}
	if err != nil {
		return StartObservation{}, err
	}
	return StartObservation{Prepared: true, Owner: &owner}, nil
}
