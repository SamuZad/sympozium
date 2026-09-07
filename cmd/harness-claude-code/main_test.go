package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrepareConfigDir_DefaultsUnderWorkspace(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	got, err := prepareConfigDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(ws, ".claude") {
		t.Fatalf("config dir = %q, want <workspace>/.claude", got)
	}
	if st, err := os.Stat(got); err != nil || !st.IsDir() {
		t.Fatalf("config dir not created: %v", err)
	}
	if os.Getenv("CLAUDE_CONFIG_DIR") != got {
		t.Fatal("CLAUDE_CONFIG_DIR not exported")
	}

	custom := filepath.Join(t.TempDir(), "custom")
	t.Setenv("CLAUDE_CONFIG_DIR", custom)
	if got, _ := prepareConfigDir(ws); got != custom {
		t.Fatalf("explicit CLAUDE_CONFIG_DIR should win, got %q", got)
	}
}

func TestEnsureWritableHome(t *testing.T) {
	ws := t.TempDir()

	writable := t.TempDir()
	t.Setenv("HOME", writable)
	if got := ensureWritableHome(ws); got != writable {
		t.Fatalf("writable HOME should be kept, got %q", got)
	}

	// Read-only HOME (as with readOnlyRootFilesystem) falls back to the workspace.
	ro := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	t.Setenv("HOME", ro)
	if got := ensureWritableHome(ws); got != ws || os.Getenv("HOME") != ws {
		t.Fatalf("read-only HOME should fall back to workspace, got %q (HOME=%q)", got, os.Getenv("HOME"))
	}

	t.Setenv("HOME", "")
	if got := ensureWritableHome(ws); got != ws {
		t.Fatalf("empty HOME should fall back to workspace, got %q", got)
	}
}

func TestWriteClaudeMD_ComposesAndRegenerates(t *testing.T) {
	configDir := t.TempDir()
	skills := t.TempDir()
	if err := os.WriteFile(filepath.Join(skills, "ops.md"), []byte("# Ops skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SKILLS_DIR", skills)

	if err := writeClaudeMD(configDir, ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(configDir, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.HasPrefix(got, "# Sympozium environment") {
		t.Fatalf("expected environment preamble first:\n%s", got)
	}
	if !strings.Contains(got, "# Ops skill") {
		t.Fatalf("skills not inlined:\n%s", got)
	}
	if strings.Contains(got, "# Agent system prompt") {
		t.Fatalf("persona prompt belongs in --append-system-prompt, not CLAUDE.md:\n%s", got)
	}

	// Skills removed on a persisted PVC must not linger.
	t.Setenv("SKILLS_DIR", t.TempDir())
	if err := writeClaudeMD(configDir, "# Overflow\n\nbig prompt"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(configDir, "CLAUDE.md"))
	if strings.Contains(string(data), "# Ops skill") {
		t.Fatal("stale skill survived regeneration")
	}
	if !strings.Contains(string(data), "# Overflow") {
		t.Fatal("extra memory not appended")
	}
}

func TestBuildSystemPrompt(t *testing.T) {
	t.Setenv("SYSTEM_PROMPT", "")
	t.Setenv("SOURCE_CHANNEL", "")
	if buildSystemPrompt(nil) != "" {
		t.Fatal("expected empty system prompt with nothing configured")
	}

	t.Setenv("SYSTEM_PROMPT", "You are the SRE persona.")
	t.Setenv("SOURCE_CHANNEL", "slack")
	t.Setenv("SOURCE_CHAT_ID", "C1")
	t.Setenv("SOURCE_THREAD_ID", "17.1")
	got := buildSystemPrompt([]string{"/workspace/attachments/log.txt"})
	for _, want := range []string{
		"# Agent system prompt",
		"You are the SRE persona.",
		"# Channel context",
		"--thread-id 17.1",
		"# Inbound attachments",
		"/workspace/attachments/log.txt",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("system prompt missing %q:\n%s", want, got)
		}
	}
	if strings.HasPrefix(got, "\n") || strings.HasSuffix(got, "\n") {
		t.Errorf("system prompt should be trimmed: %q", got)
	}
}

func TestWriteMCPConfig(t *testing.T) {
	configDir := t.TempDir()
	manifest := filepath.Join(t.TempDir(), "mcp-tools.json")
	t.Setenv("MCP_MANIFEST_PATH", manifest)
	t.Setenv("MCP_BRIDGE_URL", "")

	// Bridge advertised → config written with the http transport.
	if err := os.WriteFile(manifest, []byte(`{"tools":[{"name":"k8s_get_pods"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	path, ok, err := writeMCPConfig(configDir)
	if err != nil || !ok {
		t.Fatalf("expected MCP config, ok=%v err=%v", ok, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, data)
	}
	srv, present := cfg.MCPServers["sympozium_bridge"]
	if !present || srv.Type != "http" || srv.URL != "http://127.0.0.1:8765/mcp" {
		t.Fatalf("unexpected MCP config: %+v", cfg)
	}
	if strings.Contains(string(data), "headers") || strings.Contains(string(data), "command") {
		t.Fatalf("config must not expose remote MCP auth or stdio commands:\n%s", data)
	}

	// No manifest → stale config removed, MCP off.
	if err := os.Remove(manifest); err != nil {
		t.Fatal(err)
	}
	path2, ok, err := writeMCPConfig(configDir)
	if err != nil || ok || path2 != "" {
		t.Fatalf("expected MCP off, got path=%q ok=%v err=%v", path2, ok, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("stale MCP config should have been removed")
	}
}

func TestHasPriorSession(t *testing.T) {
	configDir := t.TempDir()
	if hasPriorSession(configDir) {
		t.Fatal("fresh config dir should have no session")
	}
	proj := filepath.Join(configDir, "projects", "-workspace")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "0d3a-session.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !hasPriorSession(configDir) {
		t.Fatal("expected transcript to be detected")
	}
}

func TestShouldContinueSession(t *testing.T) {
	tests := []struct {
		name       string
		sessionKey string
		override   string
		hasPrior   bool
		want       bool
	}{
		{"channel thread with transcript continues", "chan:slack:C1:17.1", "", true, true},
		{"channel thread without transcript starts fresh", "chan:slack:C1:17.1", "", false, false},
		{"schedule never continues by default", "sched:nightly", "", true, false},
		{"web session continues", "web:my-agent:abc", "", true, true},
		{"empty key with transcript continues", "", "", true, true},
		{"override false wins", "chan:slack:C1", "false", true, false},
		{"override off wins", "chan:slack:C1", "off", true, false},
		{"override true on schedule continues", "sched:nightly", "true", true, true},
		{"override true still needs a transcript", "sched:nightly", "true", false, false},
		{"unknown override falls back to default", "sched:nightly", "maybe", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldContinueSession(tt.sessionKey, tt.override, tt.hasPrior); got != tt.want {
				t.Errorf("shouldContinueSession(%q, %q, %v) = %v, want %v", tt.sessionKey, tt.override, tt.hasPrior, got, tt.want)
			}
		})
	}
}

func TestRunTimeout(t *testing.T) {
	if runTimeout("") != 0 || runTimeout("garbage") != 0 || runTimeout("-5s") != 0 {
		t.Fatal("invalid/empty RUN_TIMEOUT must disable the shim-side deadline")
	}
	if got := runTimeout("30m"); got != 30*time.Minute {
		t.Fatalf("runTimeout(30m) = %v", got)
	}
}

func TestMaxBudgetUSD(t *testing.T) {
	for _, bad := range []string{"", "free", "-1", "0"} {
		if got := maxBudgetUSD(bad); got != "" {
			t.Errorf("maxBudgetUSD(%q) = %q, want empty", bad, got)
		}
	}
	if got := maxBudgetUSD(" 2.50 "); got != "2.50" {
		t.Errorf("maxBudgetUSD(2.50) = %q", got)
	}
}

func TestApplyEnvIfUnset(t *testing.T) {
	t.Setenv("HARNESS_TEST_A", "operator")
	t.Setenv("HARNESS_TEST_B", "")
	applyEnvIfUnset(map[string]string{"HARNESS_TEST_A": "derived", "HARNESS_TEST_B": "derived"})
	if os.Getenv("HARNESS_TEST_A") != "operator" {
		t.Fatal("operator-set value should win")
	}
	if os.Getenv("HARNESS_TEST_B") != "derived" {
		t.Fatal("unset value should be filled")
	}
}
