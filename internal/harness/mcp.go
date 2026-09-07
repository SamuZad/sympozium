package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultMCPBridgeURL is the loopback Streamable-HTTP MCP endpoint served by
// the mcp-bridge sidecar inside the agent pod (see internal/mcpbridge).
const DefaultMCPBridgeURL = "http://127.0.0.1:8765/mcp"

type mcpToolManifest struct {
	Tools []struct {
		Name string `json:"name"`
	} `json:"tools"`
}

// ConfiguredMCPBridgeURL reports whether the mcp-bridge sidecar discovered any
// tools for this run and, if so, the local endpoint a harness should point its
// MCP client at. The sidecar owns remote MCP URLs, headers, auth secrets, tool
// filtering, and dispatch — the harness only ever sees one loopback URL.
//
// Discovery is driven by the manifest the sidecar writes (MCP_MANIFEST_PATH,
// default /ipc/tools/mcp-tools.json); an absent or empty manifest means "no
// MCP servers configured" and the harness should not advertise the bridge.
func ConfiguredMCPBridgeURL(name string) (string, bool) {
	manifestPath := EnvOr("MCP_MANIFEST_PATH", "/ipc/tools/mcp-tools.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil || len(data) == 0 {
		return "", false
	}
	var manifest mcpToolManifest
	if err := json.Unmarshal(data, &manifest); err != nil || len(manifest.Tools) == 0 {
		if err != nil {
			Logf(name, "ignoring malformed MCP manifest %s: %v", manifestPath, err)
		}
		return "", false
	}
	return EnvOr("MCP_BRIDGE_URL", DefaultMCPBridgeURL), true
}

// WaitForMCPBridge polls the local MCP endpoint with an `initialize` request
// until it answers 200 or the timeout elapses. The sidecar binds its listener
// before remote discovery completes, so this normally returns quickly; it
// exists so a harness never starts its MCP client against a port that is not
// yet bound and fails the whole run with "connection refused".
func WaitForMCPBridge(ctx context.Context, bridgeURL string, timeout time.Duration, clientName string) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":%q,"version":"1"}}}`, clientName)
	var lastErr error

	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, bridgeURL, strings.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}

		if time.Now().After(deadline) {
			return lastErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
