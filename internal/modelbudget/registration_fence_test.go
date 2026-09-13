package modelbudget

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestPostgresUnregisteredTurnFenceDoesNotGrantOrCharge(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	ctx := context.Background()
	deadline := time.Now().Add(time.Hour).Truncate(time.Second)
	id := fmt.Sprintf("registration-fence-%d", time.Now().UnixNano())
	mustRegisterRun(t, ctx, store, RunRegistration{BudgetID: id, ClusterID: "cluster", NamespaceUID: "namespace", RunUID: "run", DecisionDigest: "original", RouteDigest: "route", MaxRequests: 2, MaxOutputTokens: 1024, MaxTurns: 1, ParentDeadline: deadline})
	mustRegisterTurn(t, ctx, store, TurnRegistration{BudgetID: id, TurnID: "original", DecisionDigest: "original", MaxRequests: 2, MaxOutputTokens: 1024, Deadline: deadline})
	fence := TurnRegistration{BudgetID: id, TurnID: "never-registered", DecisionDigest: "cleanup-proof", MaxRequests: 2, MaxOutputTokens: 1024, Deadline: deadline}
	if Reason(store.RegisterTurn(ctx, fence)) != ReasonExhausted {
		t.Fatal("original turn allowance was not exhausted")
	}
	for i := 0; i < 2; i++ {
		if err := store.FenceTurnRegistration(ctx, fence); err != nil {
			t.Fatal(err)
		}
	}
	if Reason(store.RegisterTurn(ctx, fence)) != ReasonRegisterConflict {
		t.Fatal("fenced turn registration was revived")
	}
	usage, err := store.Inspect(ctx, id, fence.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if usage != (Usage{TurnClosed: true}) {
		t.Fatalf("fence granted or charged allowance: %+v", usage)
	}
	var turns int64
	if err := store.pool.QueryRow(ctx, `SELECT turns_registered FROM celln_model_budgets WHERE budget_id=$1`, id).Scan(&turns); err != nil {
		t.Fatal(err)
	}
	if turns != 1 {
		t.Fatal("no-start fence changed the original turn count")
	}
}
