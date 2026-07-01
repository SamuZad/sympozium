package mcpbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"
)

// DefaultLocalMCPAddr is the loopback endpoint exposed inside the agent pod.
const DefaultLocalMCPAddr = "127.0.0.1:8765"

type localJSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type localJSONRPCResponse struct {
	JSONRPC string             `json:"jsonrpc"`
	ID      json.RawMessage    `json:"id,omitempty"`
	Result  any                `json:"result,omitempty"`
	Error   *localJSONRPCError `json:"error,omitempty"`
}

type localJSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// StartLocalMCPServer exposes the bridge as a single local MCP Streamable HTTP
// server. Harnesses point at this endpoint instead of learning remote MCP URLs
// or auth headers; the bridge still owns discovery, filtering, and secrets.
func (b *Bridge) StartLocalMCPServer(ctx context.Context, addr string) (*http.Server, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on local MCP endpoint %s: %w", addr, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", b.handleLocalMCP)
	server := &http.Server{Handler: mux}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	go func() {
		log.Printf("Serving local MCP bridge endpoint at http://%s/mcp", addr)
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Printf("Local MCP endpoint stopped with error: %v", err)
		}
	}()

	return server, nil
}

func (b *Bridge) handleLocalMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req localJSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON-RPC request: "+err.Error(), http.StatusBadRequest)
		return
	}

	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp := localJSONRPCResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities": map[string]any{
				"tools": map[string]any{"listChanged": false},
			},
			"serverInfo": map[string]string{
				"name":    "sympozium-mcp-bridge",
				"version": "1.0.0",
			},
		}
	case "tools/list":
		resp.Result = map[string]any{"tools": b.localMCPTools()}
	case "tools/call":
		result, rpcErr := b.handleLocalMCPToolCall(r.Context(), req)
		resp.Result = result
		resp.Error = rpcErr
	default:
		resp.Error = &localJSONRPCError{Code: -32601, Message: "method not found: " + req.Method}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Mcp-Session-Id", "sympozium-mcp-bridge")
	_ = json.NewEncoder(w).Encode(resp)
}

func (b *Bridge) localMCPTools() []map[string]any {
	if b.manifest == nil {
		return []map[string]any{}
	}
	tools := make([]map[string]any, 0, len(b.manifest.Tools))
	for _, tool := range b.manifest.Tools {
		schema := tool.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		tools = append(tools, map[string]any{
			"name":        tool.Name,
			"description": tool.Description,
			"inputSchema": schema,
		})
	}
	return tools
}

func (b *Bridge) handleLocalMCPToolCall(ctx context.Context, req localJSONRPCRequest) (any, *localJSONRPCError) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, &localJSONRPCError{Code: -32602, Message: "invalid tools/call params: " + err.Error()}
	}
	if params.Name == "" {
		return nil, &localJSONRPCError{Code: -32602, Message: "tools/call requires a name"}
	}
	if len(params.Arguments) == 0 {
		params.Arguments = json.RawMessage(`{}`)
	}

	result, err := b.callTool(ctx, string(req.ID), "", params.Name, params.Arguments, nil)
	if err != nil {
		return nil, &localJSONRPCError{Code: -32000, Message: err.Error()}
	}
	return localMCPResult(result), nil
}

func localMCPResult(result *MCPResult) map[string]any {
	var content any
	if len(result.Content) > 0 && json.Unmarshal(result.Content, &content) == nil {
		// Keep the bridge-provided MCP content blocks unchanged.
	} else if result.Error != "" {
		content = []map[string]string{{"type": "text", "text": result.Error}}
	} else {
		content = []map[string]string{{"type": "text", "text": "MCP tool returned no content"}}
	}
	payload := map[string]any{"content": content}
	if !result.Success || result.IsError {
		payload["isError"] = true
	}
	return payload
}