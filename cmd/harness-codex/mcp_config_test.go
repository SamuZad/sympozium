package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

func TestWaitForMCPBridgeRetriesUntilReady(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if attempts.Add(1) < 3 {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer server.Close()

	if err := waitForMCPBridge(context.Background(), server.URL, time.Second); err != nil {
		t.Fatalf("waitForMCPBridge: %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}