package controller

import (
	"context"
	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/internal/cellnscoped"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func TestScopedParentRetainsPendingChildRecovery(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	root := &api.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "review", UID: "root-uid"}, Spec: api.AgentRunSpec{ExecutionLifecycle: "enduring"}}
	child := &api.AgentRunTurn{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "review", UID: "child-uid", Finalizers: []string{agentRunTurnFinalizer}}, Spec: api.AgentRunTurnSpec{RunName: root.Name, RunUID: string(root.UID)}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, child).Build()
	r := &AgentRunReconciler{Client: c, APIReader: c}
	if done, err := r.scopedChildrenFinalized(context.Background(), root); err != nil || done {
		t.Fatalf("pending child lost recovery root: %v %v", done, err)
	}
	child.Finalizers = nil
	if err := c.Update(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	if done, err := r.scopedChildrenFinalized(context.Background(), root); err != nil || !done {
		t.Fatalf("completed child blocked root cleanup: %v %v", done, err)
	}
}

func TestUnpreparedTurnOfClosedParentNeverEnrolls(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	root := &api.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "review", UID: "root-uid"}, Status: api.AgentRunStatus{CellnScoped: &api.CellnScopedStatus{CleanupConfirmed: true}}}
	child := &api.AgentRunTurn{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "review", UID: "child-uid", Finalizers: []string{agentRunTurnFinalizer}}, Spec: api.AgentRunTurnSpec{RunName: root.Name, RunUID: string(root.UID), Message: "do not dispatch"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(root, child).WithObjects(root, child).Build()
	r := &AgentRunTurnReconciler{Client: c, APIReader: c, ScopedDispatcher: &cellnscoped.Dispatcher{}}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(child)}); err != nil {
		t.Fatal(err)
	}
	var got api.AgentRunTurn
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(child), &got); err != nil {
		t.Fatal(err)
	}
	condition := meta.FindStatusCondition(got.Status.Conditions, "CellnTurnComplete")
	if condition == nil || condition.Reason != "CancelledBeforeAdmission" || condition.Status != metav1.ConditionTrue || got.Status.CellnScoped != nil || len(got.Finalizers) != 0 {
		t.Fatal("unprepared child acquired authority or retained a cleanup finalizer")
	}
}
