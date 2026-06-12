package controller

import (
	"context"
	"reflect"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sympoziumv1alpha1 "github.com/sympozium-ai/sympozium/api/v1alpha1"
)

func newEnsembleTestReconciler(t *testing.T, objs ...client.Object) (*EnsembleReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	if err := sympoziumv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add sympozium scheme: %v", err)
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&sympoziumv1alpha1.Ensemble{}).
		Build()
	return &EnsembleReconciler{
		Client: cl,
		Scheme: scheme,
		Log:    logr.Discard(),
	}, cl
}

// TestReconcileAgentConfig_PropagatesLifecycle covers the historical bug
// where editing a persona's Lifecycle hooks did not flow onto the existing
// Agent — the update branch ignored the field.
func TestReconcileAgentConfig_PropagatesLifecycle(t *testing.T) {
	pack := &sympoziumv1alpha1.Ensemble{
		ObjectMeta: metav1.ObjectMeta{Name: "pack", Namespace: "ns"},
		Spec:       sympoziumv1alpha1.EnsembleSpec{Enabled: true},
	}
	persona := &sympoziumv1alpha1.AgentConfigSpec{
		Name:         "lead",
		SystemPrompt: "lead",
	}
	r, _ := newEnsembleTestReconciler(t, pack)

	// Seed with the persona having no lifecycle.
	if _, err := r.reconcileAgentConfig(context.Background(), logr.Discard(), pack, persona, 0, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Now add lifecycle hooks and reconcile.
	persona.Lifecycle = &sympoziumv1alpha1.LifecycleHooks{
		PreRun: []sympoziumv1alpha1.LifecycleHookContainer{
			{Name: "init", Image: "busybox", Command: []string{"/bin/echo", "starting"}},
		},
	}
	if _, err := r.reconcileAgentConfig(context.Background(), logr.Discard(), pack, persona, 0, ""); err != nil {
		t.Fatalf("update: %v", err)
	}

	got := &sympoziumv1alpha1.Agent{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "pack-lead", Namespace: "ns"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.Agents.Default.Lifecycle == nil {
		t.Fatal("Lifecycle was not propagated to existing instance")
	}
	if !reflect.DeepEqual(got.Spec.Agents.Default.Lifecycle, persona.Lifecycle) {
		t.Errorf("Lifecycle = %+v, want %+v", got.Spec.Agents.Default.Lifecycle, persona.Lifecycle)
	}
}

func TestReconcileAgentConfig_PropagatesSubagents(t *testing.T) {
	pack := &sympoziumv1alpha1.Ensemble{
		ObjectMeta: metav1.ObjectMeta{Name: "pack", Namespace: "ns"},
		Spec:       sympoziumv1alpha1.EnsembleSpec{Enabled: true},
	}
	persona := &sympoziumv1alpha1.AgentConfigSpec{Name: "lead", SystemPrompt: "lead"}
	r, _ := newEnsembleTestReconciler(t, pack)

	if _, err := r.reconcileAgentConfig(context.Background(), logr.Discard(), pack, persona, 0, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	persona.Subagents = &sympoziumv1alpha1.SubagentsSpec{MaxDepth: 2, MaxConcurrent: 4, MaxChildrenPerAgent: 3}
	if _, err := r.reconcileAgentConfig(context.Background(), logr.Discard(), pack, persona, 0, ""); err != nil {
		t.Fatalf("update: %v", err)
	}

	got := &sympoziumv1alpha1.Agent{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "pack-lead", Namespace: "ns"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.Agents.Default.Subagents == nil || got.Spec.Agents.Default.Subagents.MaxDepth != 2 {
		t.Errorf("Subagents not propagated; got %+v", got.Spec.Agents.Default.Subagents)
	}
}

func TestReconcileAgentConfig_PropagatesPolicyRef(t *testing.T) {
	pack := &sympoziumv1alpha1.Ensemble{
		ObjectMeta: metav1.ObjectMeta{Name: "pack", Namespace: "ns"},
		Spec:       sympoziumv1alpha1.EnsembleSpec{Enabled: true},
	}
	persona := &sympoziumv1alpha1.AgentConfigSpec{Name: "lead", SystemPrompt: "lead"}
	r, _ := newEnsembleTestReconciler(t, pack)

	if _, err := r.reconcileAgentConfig(context.Background(), logr.Discard(), pack, persona, 0, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	pack.Spec.PolicyRef = "strict-policy"
	if _, err := r.reconcileAgentConfig(context.Background(), logr.Discard(), pack, persona, 0, ""); err != nil {
		t.Fatalf("update: %v", err)
	}

	got := &sympoziumv1alpha1.Agent{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "pack-lead", Namespace: "ns"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.PolicyRef != "strict-policy" {
		t.Errorf("PolicyRef = %q, want strict-policy", got.Spec.PolicyRef)
	}
}

func TestReconcileAgentConfig_PropagatesVolumes(t *testing.T) {
	pack := &sympoziumv1alpha1.Ensemble{
		ObjectMeta: metav1.ObjectMeta{Name: "pack", Namespace: "ns"},
		Spec:       sympoziumv1alpha1.EnsembleSpec{Enabled: true},
	}
	persona := &sympoziumv1alpha1.AgentConfigSpec{Name: "lead", SystemPrompt: "lead"}
	r, _ := newEnsembleTestReconciler(t, pack)

	if _, err := r.reconcileAgentConfig(context.Background(), logr.Discard(), pack, persona, 0, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	pack.Spec.Volumes = []corev1.Volume{
		{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	pack.Spec.VolumeMounts = []corev1.VolumeMount{{Name: "scratch", MountPath: "/scratch"}}
	if _, err := r.reconcileAgentConfig(context.Background(), logr.Discard(), pack, persona, 0, ""); err != nil {
		t.Fatalf("update: %v", err)
	}

	got := &sympoziumv1alpha1.Agent{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "pack-lead", Namespace: "ns"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Spec.Volumes) != 1 || got.Spec.Volumes[0].Name != "scratch" {
		t.Errorf("Volumes not propagated; got %+v", got.Spec.Volumes)
	}
	if len(got.Spec.VolumeMounts) != 1 || got.Spec.VolumeMounts[0].MountPath != "/scratch" {
		t.Errorf("VolumeMounts not propagated; got %+v", got.Spec.VolumeMounts)
	}
}

func TestReconcileAgentConfig_PropagatesAgentSandbox(t *testing.T) {
	pack := &sympoziumv1alpha1.Ensemble{
		ObjectMeta: metav1.ObjectMeta{Name: "pack", Namespace: "ns"},
		Spec:       sympoziumv1alpha1.EnsembleSpec{Enabled: true},
	}
	persona := &sympoziumv1alpha1.AgentConfigSpec{Name: "lead", SystemPrompt: "lead"}
	r, _ := newEnsembleTestReconciler(t, pack)

	if _, err := r.reconcileAgentConfig(context.Background(), logr.Discard(), pack, persona, 0, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	pack.Spec.AgentSandbox = &sympoziumv1alpha1.AgentSandboxDefaults{
		Enabled:      true,
		RuntimeClass: "gvisor",
	}
	if _, err := r.reconcileAgentConfig(context.Background(), logr.Discard(), pack, persona, 0, ""); err != nil {
		t.Fatalf("update: %v", err)
	}

	got := &sympoziumv1alpha1.Agent{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "pack-lead", Namespace: "ns"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.Agents.Default.AgentSandbox == nil {
		t.Fatal("AgentSandbox not propagated")
	}
	if got.Spec.Agents.Default.AgentSandbox.RuntimeClass != "gvisor" {
		t.Errorf("AgentSandbox.RuntimeClass = %q, want gvisor", got.Spec.Agents.Default.AgentSandbox.RuntimeClass)
	}
}

func TestReconcileAgentConfig_PropagatesModelTuning(t *testing.T) {
	pack := &sympoziumv1alpha1.Ensemble{
		ObjectMeta: metav1.ObjectMeta{Name: "pack", Namespace: "ns"},
		Spec:       sympoziumv1alpha1.EnsembleSpec{Enabled: true},
	}
	persona := &sympoziumv1alpha1.AgentConfigSpec{Name: "lead", SystemPrompt: "lead"}
	r, _ := newEnsembleTestReconciler(t, pack)

	if _, err := r.reconcileAgentConfig(context.Background(), logr.Discard(), pack, persona, 0, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}

	mt := int32(4096)
	persona.Thinking = "high"
	persona.MaxTokens = &mt
	persona.Temperature = "0.42"
	if _, err := r.reconcileAgentConfig(context.Background(), logr.Discard(), pack, persona, 0, ""); err != nil {
		t.Fatalf("update: %v", err)
	}

	got := &sympoziumv1alpha1.Agent{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "pack-lead", Namespace: "ns"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.Agents.Default.Thinking != "high" {
		t.Errorf("Thinking = %q, want high", got.Spec.Agents.Default.Thinking)
	}
	if got.Spec.Agents.Default.MaxTokens == nil || *got.Spec.Agents.Default.MaxTokens != 4096 {
		t.Errorf("MaxTokens = %v, want 4096", got.Spec.Agents.Default.MaxTokens)
	}
	if got.Spec.Agents.Default.Temperature != "0.42" {
		t.Errorf("Temperature = %q, want 0.42", got.Spec.Agents.Default.Temperature)
	}
}

func TestBuildInstance_ModelTuningOmittedWhenUnset(t *testing.T) {
	r := &EnsembleReconciler{}
	pack := &sympoziumv1alpha1.Ensemble{
		ObjectMeta: metav1.ObjectMeta{Name: "pack", Namespace: "ns"},
		Spec:       sympoziumv1alpha1.EnsembleSpec{},
	}
	persona := &sympoziumv1alpha1.AgentConfigSpec{Name: "lead", SystemPrompt: "x"}
	inst := r.buildAgent(pack, persona, "pack-lead", "")
	if inst.Spec.Agents.Default.Thinking != "" {
		t.Errorf("Thinking should be empty by default, got %q", inst.Spec.Agents.Default.Thinking)
	}
	if inst.Spec.Agents.Default.MaxTokens != nil {
		t.Errorf("MaxTokens should be nil by default, got %v", inst.Spec.Agents.Default.MaxTokens)
	}
	if inst.Spec.Agents.Default.Temperature != "" {
		t.Errorf("Temperature should be empty by default, got %q", inst.Spec.Agents.Default.Temperature)
	}
}

func TestBuildInstance_ModelTuningPropagated(t *testing.T) {
	r := &EnsembleReconciler{}
	pack := &sympoziumv1alpha1.Ensemble{
		ObjectMeta: metav1.ObjectMeta{Name: "pack", Namespace: "ns"},
		Spec:       sympoziumv1alpha1.EnsembleSpec{},
	}
	mt := int32(2048)
	persona := &sympoziumv1alpha1.AgentConfigSpec{
		Name:         "lead",
		SystemPrompt: "x",
		Thinking:     "low",
		MaxTokens:    &mt,
		Temperature:  "0.3",
	}
	inst := r.buildAgent(pack, persona, "pack-lead", "")
	if inst.Spec.Agents.Default.Thinking != "low" {
		t.Errorf("Thinking = %q, want low", inst.Spec.Agents.Default.Thinking)
	}
	if inst.Spec.Agents.Default.MaxTokens == nil || *inst.Spec.Agents.Default.MaxTokens != 2048 {
		t.Errorf("MaxTokens = %v, want 2048", inst.Spec.Agents.Default.MaxTokens)
	}
	if inst.Spec.Agents.Default.Temperature != "0.3" {
		t.Errorf("Temperature = %q, want 0.3", inst.Spec.Agents.Default.Temperature)
	}
}
