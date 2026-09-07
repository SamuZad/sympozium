package harness

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestConfiguredMCPBridgeURL(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "mcp-tools.json")
	t.Setenv("MCP_MANIFEST_PATH", manifest)
	t.Setenv("MCP_BRIDGE_URL", "")

	// No manifest → no bridge.
	if url, ok := ConfiguredMCPBridgeURL("test"); ok || url != "" {
		t.Fatalf("expected no bridge without manifest, got %q ok=%v", url, ok)
	}

	// Empty tool list → no bridge (nothing to advertise).
	if err := os.WriteFile(manifest, []byte(`{"tools":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := ConfiguredMCPBridgeURL("test"); ok {
		t.Fatal("expected no bridge for empty tool list")
	}

	// Malformed → no bridge, no panic.
	if err := os.WriteFile(manifest, []byte(`{not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := ConfiguredMCPBridgeURL("test"); ok {
		t.Fatal("expected no bridge for malformed manifest")
	}

	// Tools present → default loopback URL.
	if err := os.WriteFile(manifest, []byte(`{"tools":[{"name":"k8s_get_pods"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	url, ok := ConfiguredMCPBridgeURL("test")
	if !ok || url != DefaultMCPBridgeURL {
		t.Fatalf("expected default bridge URL, got %q ok=%v", url, ok)
	}

	// MCP_BRIDGE_URL override wins.
	t.Setenv("MCP_BRIDGE_URL", "http://127.0.0.1:9999/mcp")
	if url, _ := ConfiguredMCPBridgeURL("test"); url != "http://127.0.0.1:9999/mcp" {
		t.Fatalf("expected override URL, got %q", url)
	}
}

func TestWaitForMCPBridgeRetriesUntilReady(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if attempts.Add(1) < 3 {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer server.Close()

	if err := WaitForMCPBridge(context.Background(), server.URL, time.Second, "harness-test"); err != nil {
		t.Fatalf("WaitForMCPBridge: %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

func TestWaitForMCPBridgeTimesOut(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "never ready", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	if err := WaitForMCPBridge(context.Background(), server.URL, 300*time.Millisecond, "harness-test"); err == nil {
		t.Fatal("expected timeout error")
	}
}
