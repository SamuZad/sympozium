package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/internal/cellnauthority"
	"github.com/sympozium-ai/sympozium/internal/cellnscoped"
	"github.com/sympozium-ai/sympozium/internal/controller"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func createAuthorityAndRun(ctx context.Context, c client.Client, o options, in liveInput, providerURL, secretValue string) (*createdObjects, error) {
	profileName, policyName := resourceNames(o.namespace)
	marker := "scoped-live-" + o.namespace
	objects := &createdObjects{}
	create := func(object client.Object) error {
		if err := c.Create(ctx, object); err != nil {
			return fmt.Errorf("create %T %s/%s: %w", object, object.GetNamespace(), object.GetName(), err)
		}
		return nil
	}
	objects.evaluation = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: o.namespace, Labels: map[string]string{"sympozium.ai/celln-scoped-live": marker}}}
	if err := create(objects.evaluation); err != nil {
		return objects, err
	}
	objects.preparation = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: o.preparationNamespace, Labels: map[string]string{"sympozium.ai/celln-scoped-authority": marker}}}
	if err := create(objects.preparation); err != nil {
		return objects, err
	}
	objects.profile = &api.CellnRuntimeProfile{ObjectMeta: metav1.ObjectMeta{Name: profileName}, Spec: *in.Runtime.DeepCopy()}
	if err := create(objects.profile); err != nil {
		return objects, err
	}
	objects.policy = &api.CellnExecutionPolicy{ObjectMeta: metav1.ObjectMeta{Name: policyName}, Spec: api.CellnExecutionPolicySpec{
		NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{"sympozium.ai/celln-scoped-live": marker}},
		RuntimeProfiles:   []api.CellnExecutionPolicyRuntime{{Ref: api.CellnRuntimeProfileRef{Name: profileName, Revision: in.Runtime.Revision}}},
		Tools:             []api.CellnExecutionPolicyTool{}, Lifecycles: []string{"harness-one-shot"},
		Routes:   []api.CellnExecutionPolicyRoute{{Provider: in.Model.Provider, Protocol: in.Model.Protocol, Models: []string{in.Model.Name}, EndpointOrigins: []string{providerURL}, Auth: "secret"}},
		Ceilings: api.CellnExecutionPolicyCeilings{MaxTurns: 1, MaxModelRequests: in.Expected.ProviderCalls, MaxOutputTokens: in.Expected.ReservedOutputTokens, MaxParentLeaseSeconds: 60, MaxTurnSeconds: 60},
	}}
	if err := create(objects.policy); err != nil {
		return objects, err
	}
	runtimeWrapper := &api.AgentRuntime{ObjectMeta: metav1.ObjectMeta{Namespace: o.namespace, Name: "runtime"}, Spec: api.AgentRuntimeSpec{CellnProfileRef: &api.CellnRuntimeProfileRef{Name: profileName, Revision: in.Runtime.Revision}}}
	agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: o.namespace, Name: "agent"}, Spec: api.AgentSpec{RuntimeRef: runtimeWrapper.Name, Agents: api.AgentsSpec{Default: api.AgentConfig{Model: "unused-native-route"}}}}
	objects.secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: o.namespace, Name: "model-credential"}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"OPENAI_API_KEY": []byte(secretValue)}}
	connection := &api.ModelConnection{ObjectMeta: metav1.ObjectMeta{Namespace: o.namespace, Name: "model"}, Spec: api.ModelConnectionSpec{Provider: in.Model.Provider, Protocol: in.Model.Protocol, Endpoint: providerURL + "/v1/chat/completions", SecretRef: objects.secret.Name, Models: []string{in.Model.Name}, AllowInsecure: true}}
	for _, object := range []client.Object{runtimeWrapper, agent, objects.secret, connection} {
		if err := create(object); err != nil {
			return objects, err
		}
	}
	objects.run = &api.AgentRun{ObjectMeta: metav1.ObjectMeta{Namespace: o.namespace, Name: "scoped-live"}, Spec: api.AgentRunSpec{
		AgentRef: agent.Name, AgentID: "default", SessionKey: "scoped-live", Task: api.NewStringTask(in.Task), SystemPrompt: in.SystemPrompt,
		Model: api.ModelSpec{ConnectionRef: connection.Name, Model: in.Model.Name}, Backend: "celln", ExecutionLifecycle: "one-shot",
		CellnSelection: &api.CellnCatalogueSelection{RuntimeRef: runtimeWrapper.Name, ToolRefs: []api.CellnCatalogueToolRef{}, ClusterToolRefs: []api.ClusterCellnToolRef{}}, Cleanup: "delete",
	}}
	if err := create(objects.run); err != nil {
		return objects, err
	}
	return objects, nil
}

func driveToTerminal(ctx context.Context, reconciler *controller.AgentRunReconciler, dispatcher *cellnscoped.Dispatcher, c client.Client, original *api.AgentRun) (api.AgentRun, cellnscoped.OperationStatus, *cellnauthority.FinalizedPreparation, error) {
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(original)}
	for {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			return api.AgentRun{}, cellnscoped.OperationStatus{}, nil, diagnostic(ctx, c, request.NamespacedName, "reconcile to terminal", err)
		}
		var current api.AgentRun
		if err := c.Get(ctx, request.NamespacedName, &current); err != nil {
			return current, cellnscoped.OperationStatus{}, nil, err
		}
		if current.Status.Phase == api.AgentRunPhaseFailed {
			return current, cellnscoped.OperationStatus{}, nil, diagnostic(ctx, c, request.NamespacedName, "scoped execution failed", errors.New(current.Status.Error))
		}
		if current.Status.Phase == api.AgentRunPhaseSucceeded {
			if current.Status.CellnScoped == nil || current.Status.CellnScoped.ReceiverID == "" || current.Status.CellnScoped.ReceiptDigest == "" {
				return current, cellnscoped.OperationStatus{}, nil, errors.New("terminal run lacks scoped owner/receipt identity")
			}
			prepared, err := dispatcher.Store.Load(ctx, current.Status.CellnScoped.PreparationName)
			if err != nil {
				return current, cellnscoped.OperationStatus{}, nil, err
			}
			final, err := dispatcher.Store.LoadFinal(ctx, current.Status.CellnScoped.DecisionName, prepared)
			if err != nil {
				return current, cellnscoped.OperationStatus{}, nil, err
			}
			_, execution, model, err := dispatcher.StartTokens(final)
			if err != nil {
				return current, cellnscoped.OperationStatus{}, final, fmt.Errorf("issue duplicate-start permits inside original window: %w", err)
			}
			duplicate, err := dispatcher.Start(ctx, current.Status.CellnScoped.ReceiverID, execution, model)
			if err != nil {
				return current, duplicate, final, fmt.Errorf("native duplicate-start recovery: %w", err)
			}
			if duplicate.ID != current.Status.CellnScoped.ReceiverID || duplicate.Owner != current.Status.CellnScoped.Owner || duplicate.Phase != "Succeeded" {
				return current, duplicate, final, fmt.Errorf("duplicate start did not recover original terminal owner: idMatch=%t ownerMatch=%t phase=%s", duplicate.ID == current.Status.CellnScoped.ReceiverID, duplicate.Owner == current.Status.CellnScoped.Owner, duplicate.Phase)
			}
			return current, duplicate, final, nil
		}
		select {
		case <-ctx.Done():
			return current, cellnscoped.OperationStatus{}, nil, diagnostic(ctx, c, request.NamespacedName, "terminal timeout", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func driveCleanup(ctx context.Context, reconciler *controller.AgentRunReconciler, c client.Client, original *api.AgentRun) (api.AgentRun, error) {
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(original)}
	for {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			return api.AgentRun{}, diagnostic(ctx, c, request.NamespacedName, "reconcile cleanup", err)
		}
		var current api.AgentRun
		if err := c.Get(ctx, request.NamespacedName, &current); err != nil {
			return current, err
		}
		if current.Status.CellnScoped != nil && current.Status.CellnScoped.CleanupConfirmed && !containsFinalizer(current.Finalizers) {
			return current, nil
		}
		select {
		case <-ctx.Done():
			return current, diagnostic(ctx, c, request.NamespacedName, "cleanup timeout", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func containsFinalizer(values []string) bool {
	return slices.Contains(values, "sympozium.ai/agentrun-finalizer")
}

func diagnostic(ctx context.Context, c client.Client, key types.NamespacedName, stage string, cause error) error {
	var run api.AgentRun
	if err := c.Get(ctx, key, &run); err != nil {
		return fmt.Errorf("%s: %v (status unavailable: %v)", stage, cause, err)
	}
	condition := "none"
	for _, item := range run.Status.Conditions {
		if item.Type == "CellnScopedExecution" {
			condition = item.Reason + ":" + item.Message
		}
	}
	native := "none"
	if run.Status.CellnScoped != nil {
		native = run.Status.CellnScoped.NativePhase
	}
	return fmt.Errorf("%s: %v (phase=%s native=%s condition=%s)", stage, cause, run.Status.Phase, native, condition)
}

func assertProtectedRecords(ctx context.Context, c client.Client, namespace string, run api.AgentRun, providerSecret string) error {
	if run.Status.CellnScoped == nil {
		return errors.New("scoped status missing while checking protected records")
	}
	for _, name := range []string{run.Status.CellnScoped.PreparationName, run.Status.CellnScoped.DecisionName} {
		var record corev1.ConfigMap
		if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &record); err != nil {
			return fmt.Errorf("protected record %q unavailable: %w", name, err)
		}
		if record.Immutable == nil || !*record.Immutable || record.Namespace != namespace {
			return fmt.Errorf("protected record %q is not immutable in its dedicated namespace", name)
		}
		for _, value := range record.Data {
			if strings.Contains(value, providerSecret) || strings.Contains(value, "Bearer ") {
				return fmt.Errorf("protected record %q contains credential material", name)
			}
		}
	}
	return nil
}

func deleteRunThroughController(ctx context.Context, reconciler *controller.AgentRunReconciler, c client.Client, original *api.AgentRun) error {
	if original == nil || original.UID == "" {
		return nil
	}
	key := client.ObjectKeyFromObject(original)
	var current api.AgentRun
	if err := c.Get(ctx, key, &current); apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	uid := current.UID
	if err := c.Delete(ctx, &current, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("request failed-run deletion: %w", err)
	}
	return wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 20*time.Second, true, func(poll context.Context) (bool, error) {
		if _, err := reconciler.Reconcile(poll, ctrl.Request{NamespacedName: key}); err != nil {
			return false, err
		}
		var probe api.AgentRun
		err := c.Get(poll, key, &probe)
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	})
}

func cleanupCreated(ctx context.Context, c client.Client, objects *createdObjects) error {
	if objects == nil {
		return nil
	}
	var failures []string
	remove := func(object client.Object) {
		if object == nil || object.GetUID() == "" {
			return
		}
		uid := object.GetUID()
		if err := c.Delete(ctx, object, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
			failures = append(failures, fmt.Sprintf("%s/%s: %v", object.GetNamespace(), object.GetName(), err))
		}
	}
	// The run should already have completed native/gateway cleanup. Namespace
	// deletion is only for harness-created Kubernetes fixtures.
	remove(objects.run)
	remove(objects.policy)
	remove(objects.profile)
	remove(objects.evaluation)
	remove(objects.preparation)
	if len(failures) != 0 {
		return fmt.Errorf("created-resource cleanup failed: %s", strings.Join(failures, "; "))
	}
	for _, object := range []client.Object{objects.run, objects.policy, objects.profile, objects.evaluation, objects.preparation} {
		if object == nil || object.GetUID() == "" {
			continue
		}
		key := client.ObjectKeyFromObject(object)
		err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 20*time.Second, true, func(poll context.Context) (bool, error) {
			probe, ok := object.DeepCopyObject().(client.Object)
			if !ok {
				return false, errors.New("created object is not a Kubernetes client object")
			}
			err := c.Get(poll, key, probe)
			return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
		})
		if err != nil {
			return fmt.Errorf("created-resource deletion unconfirmed for %s/%s: %w", key.Namespace, key.Name, err)
		}
	}
	return nil
}
