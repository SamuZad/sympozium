package controller

import (
	"context"
	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func TestScopedPrestartCancellationIsNotTeardown(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	now := metav1.Now()
	run := &api.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "cancelled", Namespace: "review", DeletionTimestamp: &now, Finalizers: []string{"example.test/evidence"}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.AgentRun{}).WithObjects(run).Build()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
		t.Fatal(err)
	}
	r := &AgentRunReconciler{Client: c, ScopedOnly: true}
	if _, err := r.finishScopedOnly(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
		t.Fatal(err)
	}
	if run.Status.CellnScoped != nil || len(run.Status.Conditions) != 1 || run.Status.Conditions[0].Reason != "CancelledBeforeAdmission" || run.Status.Conditions[0].Status != metav1.ConditionFalse || run.Finalizers[0] != "example.test/evidence" {
		t.Fatal("prestart cancellation fabricated teardown or dropped evidence")
	}
}
