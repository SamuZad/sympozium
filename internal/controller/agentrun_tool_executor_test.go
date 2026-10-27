package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sympoziumv1alpha1 "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/pkg/sidecartools"
)

func toolExecutorTestSidecars() []resolvedSidecar {
	return []resolvedSidecar{
		{skillPackName: "kubectl-stock", sidecar: sympoziumv1alpha1.SkillSidecar{
			Image: "bitnami/kubectl:1.31",
		}},
		{skillPackName: "custom", sidecar: sympoziumv1alpha1.SkillSidecar{
			Image:   "ghcr.io/example/custom:v1",
			Command: []string{"/app/start.sh"},
		}},
	}
}

// Every skill sidecar gets the script mounted read-only, and no sidecar's
// command changes: images keep their own entrypoint unless the SkillPack opts
// in by naming the mounted script.
func TestBuildContainers_SidecarsMountToolExecutor(t *testing.T) {
	r := &AgentRunReconciler{}
	run := newTestRun()
	containers, _, err := r.buildContainers(run, false, nil, toolExecutorTestSidecars(), nil, nil)
	if err != nil {
		t.Fatalf("buildContainers: %v", err)
	}

	wantCommand := map[string][]string{"skill-kubectl-stock": nil, "skill-custom": {"/app/start.sh"}}
	for name, want := range wantCommand {
		c := containerByName(containers, name)
		if c == nil {
			t.Fatalf("missing sidecar container %q", name)
		}
		mounted := false
		for _, m := range c.VolumeMounts {
			if m.Name == toolExecutorVolumeName && m.MountPath == sidecartools.ExecutorMountDir && m.ReadOnly {
				mounted = true
			}
		}
		if !mounted {
			t.Errorf("%s: tool-executor not mounted read-only at %s", name, sidecartools.ExecutorMountDir)
		}
		if len(c.Command) != len(want) || (len(want) > 0 && c.Command[0] != want[0]) {
			t.Errorf("%s: command = %v, want %v (the mount must not change entrypoints)", name, c.Command, want)
		}
	}
	for _, m := range containers[0].VolumeMounts {
		if m.Name == toolExecutorVolumeName {
			t.Error("the agent container should not mount the tool-executor")
		}
	}
}

func TestBuildVolumes_ToolExecutorOnlyWithSidecars(t *testing.T) {
	r := &AgentRunReconciler{}
	run := newTestRun()

	for _, v := range r.buildVolumes(run, false, nil, nil) {
		if v.Name == toolExecutorVolumeName {
			t.Fatal("tool-executor volume added to a pod without skill sidecars")
		}
	}

	var vol *corev1.Volume
	for _, v := range r.buildVolumes(run, false, toolExecutorTestSidecars(), nil) {
		if v.Name == toolExecutorVolumeName {
			vol = &v
		}
	}
	if vol == nil || vol.ConfigMap == nil {
		t.Fatalf("missing tool-executor ConfigMap volume, got %+v", vol)
	}
	if vol.ConfigMap.Name != toolExecutorConfigMapName(run) {
		t.Errorf("volume ConfigMap = %q, want %q", vol.ConfigMap.Name, toolExecutorConfigMapName(run))
	}
	if vol.ConfigMap.DefaultMode == nil || *vol.ConfigMap.DefaultMode&0o111 == 0 {
		t.Errorf("script must be mounted executable, got mode %v", vol.ConfigMap.DefaultMode)
	}
}

func TestEnsureToolExecutorConfigMap(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := sympoziumv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	run := newTestRun()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(run).Build()
	r := &AgentRunReconciler{Client: cl, Scheme: scheme}

	ctx := context.Background()
	if err := r.ensureToolExecutorConfigMap(ctx, run); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if err := r.ensureToolExecutorConfigMap(ctx, run); err != nil {
		t.Fatalf("second ensure must tolerate the existing ConfigMap: %v", err)
	}

	var cm corev1.ConfigMap
	if err := cl.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: toolExecutorConfigMapName(run)}, &cm); err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}
	if cm.Data[sidecartools.ExecutorScriptName] != sidecartools.ExecutorScript {
		t.Error("ConfigMap does not hold the canonical tool-executor script")
	}
	if cm.Immutable == nil || !*cm.Immutable {
		t.Error("ConfigMap should be immutable")
	}
	if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].Name != run.Name {
		t.Errorf("ConfigMap should be owned by the run, got %+v", cm.OwnerReferences)
	}
}

func TestIsReservedVolumeName_ToolExecutor(t *testing.T) {
	if !isReservedVolumeName(toolExecutorVolumeName) {
		t.Errorf("%q must be reserved", toolExecutorVolumeName)
	}
}
