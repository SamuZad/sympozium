package cellnauthority

import (
	"context"
	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"testing"
	"time"
)

type preparationUIDClient struct{ client.Client }

func (c preparationUIDClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*corev1.ConfigMap); ok {
		obj.SetUID("test-preparation-uid")
	}
	return c.Client.Create(ctx, obj, opts...)
}

func TestPreparationRetainsOriginalClockAndSource(t *testing.T) {
	f := newPlatformFixture(t, "tenant-a", true)
	store := PreparedStore{Writer: preparationUIDClient{f.client}, Reader: f.spy, Namespace: "control-plane", Resolver: f.resolver}
	request := PlatformResolveRequest{ClusterID: "cluster", Now: f.now}
	first, err := store.Prepare(context.Background(), f.runKey, request)
	if err != nil {
		t.Fatal(err)
	}
	request.Now = request.Now.Add(time.Hour)
	next, err := store.Prepare(context.Background(), f.runKey, request)
	if err != nil {
		t.Fatal(err)
	}
	if next.UID != first.UID || next.Operation.Resolution.Decision.Budget != first.Operation.Resolution.Decision.Budget || next.Operation.Resolution.Decision.Windows != first.Operation.Resolution.Decision.Windows {
		t.Fatal("preparation recovery changed original identity, clock or allowance")
	}
	if next.Operation.Resolution.Decision.Route.CredentialSourceRef == nil || f.spy.secretReads != 0 {
		t.Fatal("credential reference was lost or a Secret was read")
	}
	var run api.AgentRun
	if err := f.client.Get(context.Background(), f.runKey, &run); err != nil {
		t.Fatal(err)
	}
	run.Spec.Task = api.NewStringTask("changed task")
	if err := f.client.Update(context.Background(), &run); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Prepare(context.Background(), f.runKey, request); err == nil {
		t.Fatal("changed run reused prepared authority")
	}
}

func TestPreparedMaterialRejectsSubstitution(t *testing.T) {
	f := newPlatformFixture(t, "tenant-a", false)
	resolved, err := f.resolver.Resolve(context.Background(), f.runKey, PlatformResolveRequest{ClusterID: "cluster", Now: f.now})
	if err != nil {
		t.Fatal(err)
	}
	op := PreparedOperation{APIVersion: preparedVersion, Resolution: *resolved}
	if err := ValidatePreparedBindings(op); err != nil {
		t.Fatal(err)
	}
	op.Resolution.Execution.Payload = "replacement"
	if err := ValidatePreparedBindings(op); err == nil {
		t.Fatal("changed payload accepted")
	}
}
