package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	sympoziumv1alpha1 "github.com/sympozium-ai/sympozium/api/v1alpha1"
)

// envValue returns the value of the first env var with the given name on
// container c, or empty string if not present.
func envValue(c corev1.Container, name string) string {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

func TestAgentServiceAccountName(t *testing.T) {
	tests := []struct {
		name     string
		agentRun *sympoziumv1alpha1.AgentRun
		want     string
	}{
		{
			name:     "with AgentRef",
			agentRun: &sympoziumv1alpha1.AgentRun{Spec: sympoziumv1alpha1.AgentRunSpec{AgentRef: "alice"}},
			want:     "alice-agent",
		},
		{
			name:     "without AgentRef falls back to legacy shared SA",
			agentRun: &sympoziumv1alpha1.AgentRun{Spec: sympoziumv1alpha1.AgentRunSpec{}},
			want:     "sympozium-agent",
		},
		{
			name:     "nil AgentRun falls back to legacy",
			agentRun: nil,
			want:     "sympozium-agent",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := agentServiceAccountName(tt.agentRun)
			if got != tt.want {
				t.Errorf("agentServiceAccountName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildContainers_MemoryServerURL_Injected(t *testing.T) {
	r := &AgentRunReconciler{
		MemoryServerURL: "http://release-memory-server.sympozium-system.svc:8080",
	}
	cs, _ := r.buildContainers(newTestRun(), true /* memoryEnabled */, nil, nil, nil)

	got := envValue(cs[0], "MEMORY_SERVER_URL")
	if got != "http://release-memory-server.sympozium-system.svc:8080" {
		t.Errorf("MEMORY_SERVER_URL = %q, want the configured URL", got)
	}
}

func TestBuildContainers_MemoryServerURL_OmittedWhenMemoryDisabled(t *testing.T) {
	r := &AgentRunReconciler{
		MemoryServerURL: "http://release-memory-server.sympozium-system.svc:8080",
	}
	cs, _ := r.buildContainers(newTestRun(), false /* memoryEnabled */, nil, nil, nil)

	if got := envValue(cs[0], "MEMORY_SERVER_URL"); got != "" {
		t.Errorf("MEMORY_SERVER_URL should be unset when memory disabled, got %q", got)
	}
}

func TestBuildContainers_MemoryServerURL_OmittedWhenURLUnset(t *testing.T) {
	r := &AgentRunReconciler{} // MemoryServerURL empty
	cs, _ := r.buildContainers(newTestRun(), true, nil, nil, nil)

	if got := envValue(cs[0], "MEMORY_SERVER_URL"); got != "" {
		t.Errorf("MEMORY_SERVER_URL should be unset when controller has no URL, got %q", got)
	}
}
