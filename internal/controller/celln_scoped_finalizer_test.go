package controller

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/internal/cellnscoped"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

type noLegacyList struct{ client.Client }

func (c noLegacyList) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return fmt.Errorf("legacy listing is forbidden")
}
func TestScopedOnlyPreservesObserverWithoutLegacyCleanup(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		t.Run(fmt.Sprint(deleting), func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = api.AddToScheme(scheme)
			run := &api.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "review", Namespace: "review", Finalizers: []string{agentRunFinalizer, "example.test/evidence"}}, Spec: api.AgentRunSpec{Backend: "celln", ExecutionLifecycle: "one-shot"}, Status: api.AgentRunStatus{Phase: api.AgentRunPhaseSucceeded, CellnScoped: &api.CellnScopedStatus{CleanupConfirmed: true}}}
			if deleting {
				now := metav1.Now()
				run.DeletionTimestamp = &now
			}
			c := noLegacyList{fake.NewClientBuilder().WithScheme(scheme).WithObjects(run).Build()}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
				t.Fatal(err)
			}
			r := &AgentRunReconciler{Client: c, ScopedOnly: true, ScopedDispatcher: &cellnscoped.Dispatcher{}}
			if deleting {
				if _, err := r.reconcileDelete(context.Background(), logr.Discard(), run); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := r.reconcileCompleted(context.Background(), logr.Discard(), run); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
				t.Fatal(err)
			}
			if len(run.Finalizers) != 1 || run.Finalizers[0] != "example.test/evidence" {
				t.Fatalf("foreign finalizer changed: %v", run.Finalizers)
			}
		})
	}
}
