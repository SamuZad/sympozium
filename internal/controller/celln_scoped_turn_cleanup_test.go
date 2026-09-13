package controller

import (
	"context"
	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func TestScopedTurnTerminalCleanupDoesNotReadmit(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancelled-before-admission", true: "confirmed-cleanup"}[completed], func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := api.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			turn := &api.AgentRunTurn{ObjectMeta: metav1.ObjectMeta{Name: "turn", Namespace: "tenant", UID: "turn-uid", Finalizers: []string{agentRunTurnFinalizer}}}
			if completed {
				turn.Status.CellnScoped = &api.CellnScopedStatus{CleanupConfirmed: true}
			} else {
				turn.Spec.CancelRequested = true
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.AgentRunTurn{}).WithObjects(turn).Build()
			r := &AgentRunTurnReconciler{Client: c, APIReader: c}
			key := types.NamespacedName{Namespace: turn.Namespace, Name: turn.Name}
			for n := 0; n < 3; n++ {
				if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
					t.Fatal(err)
				}
				var got api.AgentRunTurn
				if err := c.Get(context.Background(), key, &got); err != nil {
					t.Fatal(err)
				}
				if len(got.Finalizers) != 0 {
					t.Fatal("terminal turn regained its finalizer")
				}
				if !completed {
					condition := meta.FindStatusCondition(got.Status.Conditions, "CellnTurnComplete")
					if condition == nil || condition.Reason != "CancelledBeforeAdmission" || got.Status.CellnScoped != nil {
						t.Fatal("pre-admission cancellation created native state")
					}
				}
			}
		})
	}
}
