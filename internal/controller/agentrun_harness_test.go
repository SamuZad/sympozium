package controller

import (
	"testing"
)

func TestHarnessImage(t *testing.T) {
	r := &AgentRunReconciler{ImageTag: "v0.1.0"}
	tests := []struct {
		name    string
		harness string
		want    string
	}{
		{"empty defaults to agent-runner", "", "ghcr.io/sympozium-ai/sympozium/agent-runner:v0.1.0"},
		{"explicit agent-runner", "agent-runner", "ghcr.io/sympozium-ai/sympozium/agent-runner:v0.1.0"},
		{"codex", "codex", "ghcr.io/sympozium-ai/sympozium/harness-codex:v0.1.0"},
		{"claude-code", "claude-code", "ghcr.io/sympozium-ai/sympozium/harness-claude-code:v0.1.0"},
		{"unknown falls back to agent-runner", "made-up", "ghcr.io/sympozium-ai/sympozium/agent-runner:v0.1.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.harnessImage(tt.harness)
			if got != tt.want {
				t.Errorf("harnessImage(%q) = %q, want %q", tt.harness, got, tt.want)
			}
		})
	}
}

func TestHarnessImage_RespectsRegistryOverride(t *testing.T) {
	t.Setenv("SYMPOZIUM_IMAGE_REGISTRY", "my.registry.example/sym")
	r := &AgentRunReconciler{ImageTag: "v9.9.9"}
	got := r.harnessImage("codex")
	want := "my.registry.example/sym/harness-codex:v9.9.9"
	if got != want {
		t.Errorf("harnessImage = %q, want %q", got, want)
	}
}
