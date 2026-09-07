package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteConfigTOMLUsesLocalMCPBridgeAdapter(t *testing.T) {
	dir := t.TempDir()
	codexHome := filepath.Join(dir, "codex")
	manifestPath := filepath.Join(dir, "mcp-tools.json")
	if err := os.WriteFile(manifestPath, []byte(`{"tools":[{"name":"k8s_get_pods"}]}`), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("MCP_MANIFEST_PATH", manifestPath)
	t.Setenv("MCP_BRIDGE_URL", "http://127.0.0.1:8765/mcp")
	t.Setenv("MODEL_PROVIDER", "openai")
	t.Setenv("MODEL_NAME", "gpt-5")

	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatalf("mkdir codex home: %v", err)
	}
	if err := writeConfigTOML(codexHome); err != nil {
		t.Fatalf("writeConfigTOML: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	config := string(data)
	if !strings.Contains(config, "[mcp_servers.sympozium_bridge]") {
		t.Fatalf("config missing bridge MCP server:\n%s", config)
	}
	if !strings.Contains(config, `url = "http://127.0.0.1:8765/mcp"`) {
		t.Fatalf("config missing local MCP bridge URL:\n%s", config)
	}
	if strings.Contains(config, "bearer_token_env_var") || strings.Contains(config, "command =") || strings.Contains(config, "args =") {
		t.Fatalf("config should not expose direct MCP auth or stdio adapter command to codex:\n%s", config)
	}
}

func TestWriteAgentsMDComposesSharedSections(t *testing.T) {
	codexHome := t.TempDir()
	skills := t.TempDir()
	if err := os.WriteFile(filepath.Join(skills, "ops.md"), []byte("# Ops skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SKILLS_DIR", skills)
	t.Setenv("SYSTEM_PROMPT", "You are the codex persona.")
	t.Setenv("SOURCE_CHANNEL", "slack")
	t.Setenv("SOURCE_CHAT_ID", "C1")
	t.Setenv("SOURCE_THREAD_ID", "")

	if err := writeAgentsMD(codexHome, []string{"/workspace/attachments/in.csv"}); err != nil {
		t.Fatalf("writeAgentsMD: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(codexHome, "AGENTS.md"))
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	got := string(data)
	// Order matters: skills, then system prompt, then channel frame, then attachments.
	idx := func(s string) int { return strings.Index(got, s) }
	if !(idx("# Ops skill") < idx("# Agent system prompt") &&
		idx("# Agent system prompt") < idx("# Channel context") &&
		idx("# Channel context") < idx("# Inbound attachments")) {
		t.Fatalf("sections out of order or missing:\n%s", got)
	}
	if !strings.Contains(got, "/workspace/attachments/in.csv") {
		t.Fatalf("missing inbound attachment listing:\n%s", got)
	}
}
