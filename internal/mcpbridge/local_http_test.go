package mcpbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestLocalMCPHTTPRoutesThroughBridgeClients(t *testing.T) {
	callHandler := func(params MCPToolCallParams) (*MCPToolCallResult, *JSONRPCError) {
		if params.Name != "get_pods" {
			return nil, &JSONRPCError{Code: -32602, Message: "unexpected tool: " + params.Name}
		}
		return &MCPToolCallResult{
			Content: []MCPContent{{Type: "text", Text: "pods ok"}},
		}, nil
	}

	remote := mockMCPServer(t, []MCPTool{{
		Name:        "get_pods",
		Description: "List pods",
		InputSchema: map[string]any{"type": "object"},
	}}, callHandler)
	defer remote.Close()

	cfg := &ServersConfig{Servers: []ServerConfig{{
		Name:        "k8s",
		URL:         remote.URL,
		ToolsPrefix: "k8s",
		Timeout:     10,
		Transport:   "streamable-http",
	}}}
	bridge := NewBridge(cfg, t.TempDir(), filepath.Join(t.TempDir(), "mcp-tools.json"), "test-run")
	manifest, err := bridge.discoverTools(context.Background())
	if err != nil {
		t.Fatalf("discoverTools: %v", err)
	}
	bridge.manifest = manifest
	bridge.markReady()

	local := httptest.NewServer(http.HandlerFunc(bridge.handleLocalMCP))
	defer local.Close()

	listResp := postLocalMCP(t, local.URL, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
	})
	tools := listResp.Result["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools/list returned %d tools, want 1", len(tools))
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "k8s_get_pods" {
		t.Fatalf("tool name = %v, want k8s_get_pods", tool["name"])
	}

	callResp := postLocalMCP(t, local.URL, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "k8s_get_pods",
			"arguments": map[string]any{"namespace": "default"},
		},
	})
	content := callResp.Result["content"].([]any)
	text := content[0].(map[string]any)["text"]
	if text != "pods ok" {
		t.Fatalf("tools/call text = %v, want pods ok", text)
	}
}

type localMCPTestResponse struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id"`
	Result  map[string]any `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func postLocalMCP(t *testing.T, url string, payload map[string]any) localMCPTestResponse {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post local MCP: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var decoded localMCPTestResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if decoded.Error != nil {
		t.Fatalf("JSON-RPC error %d: %s", decoded.Error.Code, decoded.Error.Message)
	}
	return decoded
}