package mcpbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

var bridgeTracer = otel.Tracer("sympozium.ai/mcp-bridge")
var bridgeMeter = otel.Meter("sympozium.ai/mcp-bridge")

var (
	mcpToolCalls, _    = bridgeMeter.Int64Counter("mcp.bridge.tool_calls", metric.WithUnit("{call}"), metric.WithDescription("MCP tool calls dispatched"))
	mcpToolErrors, _   = bridgeMeter.Int64Counter("mcp.bridge.tool_errors", metric.WithUnit("{error}"), metric.WithDescription("MCP tool call errors"))
	mcpToolDuration, _ = bridgeMeter.Float64Histogram("mcp.bridge.tool_duration_ms", metric.WithUnit("ms"), metric.WithDescription("MCP tool call duration"))
)

// maxConcurrent is the maximum number of concurrent MCP requests.
const maxConcurrent = 10

// Bridge is the MCP bridge sidecar process.
type Bridge struct {
	config       *ServersConfig
	ipcPath      string
	manifestPath string
	agentRunID   string
	clients      map[string]*Client // server name -> client
	toolIndex    map[string]string  // prefixed tool name -> server name
	prefixIndex  map[string]string  // tools prefix -> server name
	manifest     *MCPToolManifest
	processed    sync.Map           // dedup fsnotify events

	// ready is closed once tool discovery has completed and the manifest has
	// been written. Handlers that read discovered state (clients, toolIndex,
	// manifest) wait on it; closing the channel also establishes the
	// happens-before relationship that makes those reads safe without a mutex.
	ready     chan struct{}
	readyOnce sync.Once
}

// NewBridge creates a new MCP bridge.
func NewBridge(cfg *ServersConfig, ipcPath, manifestPath, agentRunID string) *Bridge {
	prefixIdx := make(map[string]string, len(cfg.Servers))
	for _, s := range cfg.Servers {
		prefixIdx[s.ToolsPrefix] = s.Name
	}

	return &Bridge{
		ready: make(chan struct{}),
		config:       cfg,
		ipcPath:      ipcPath,
		manifestPath: manifestPath,
		agentRunID:   agentRunID,
		clients:      make(map[string]*Client),
		toolIndex:    make(map[string]string),
		prefixIndex:  prefixIdx,
	}
}

// Run starts the MCP bridge. The local MCP HTTP endpoint is bound *before*
// tool discovery so harnesses can complete their initialize handshake
// immediately: discovery can take tens of seconds when a remote MCP server is
// slow to come up (up to 6 retries × 10s per server), and if the listener only
// bound afterwards the harness would hit "connection refused" and fail the run.
// Discovery then runs in the background; tools/list and tools/call block on
// b.ready until it finishes, so callers still observe the full tool set.
func (b *Bridge) Run(ctx context.Context) error {
	ctx, span := bridgeTracer.Start(ctx, "mcp-bridge.run",
		trace.WithAttributes(attribute.String("agent_run_id", b.agentRunID)),
	)
	defer span.End()

	// Phase 1: Bind and serve the local MCP endpoint up front so the harness
	// handshake succeeds within its wait window.
	localMCPAddr := strings.TrimSpace(os.Getenv("MCP_BRIDGE_LISTEN_ADDR"))
	if localMCPAddr == "" {
		localMCPAddr = DefaultLocalMCPAddr
	}
	if !strings.EqualFold(localMCPAddr, "disabled") && !strings.EqualFold(localMCPAddr, "off") {
		server, err := b.StartLocalMCPServer(ctx, localMCPAddr)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "local MCP endpoint failed")
			return err
		}
		if server != nil {
			defer server.Shutdown(context.Background())
		}
	}

	// Phase 2: Discover tools in the background, write the manifest, then signal
	// readiness. Reads of clients/toolIndex/manifest are guarded by b.ready.
	go func() {
		defer b.markReady()
		manifest, err := b.discoverTools(ctx)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "tool discovery failed")
			log.Printf("MCP tool discovery failed, continuing with no tools: %v", err)
			return
		}
		span.SetAttributes(attribute.Int("mcp.tools_discovered", len(manifest.Tools)))
		b.manifest = manifest
		if err := WriteManifest(b.manifestPath, manifest); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "manifest write failed")
			log.Printf("Failed to write MCP tool manifest: %v", err)
			return
		}
		log.Printf("Wrote tool manifest with %d tools to %s", len(manifest.Tools), b.manifestPath)
	}()

	// Phase 3: Watch for MCP requests and dispatch
	return b.watchAndDispatch(ctx)
}

// markReady signals that tool discovery has completed. It is idempotent and
// safe to call concurrently.
func (b *Bridge) markReady() {
	b.readyOnce.Do(func() { close(b.ready) })
}

// waitReady blocks until tool discovery has completed (b.ready closed), the
// context is cancelled, or the timeout elapses. It returns true only when the
// bridge became ready. A non-positive timeout waits until ready or ctx done.
func (b *Bridge) waitReady(ctx context.Context, timeout time.Duration) bool {
	if b.ready == nil {
		return true
	}
	select {
	case <-b.ready:
		return true
	default:
	}
	if timeout <= 0 {
		select {
		case <-b.ready:
			return true
		case <-ctx.Done():
			return false
		}
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-b.ready:
		return true
	case <-ctx.Done():
		return false
	case <-t.C:
		return false
	}
}

// watchAndDispatch watches the IPC tools directory for MCP request files
// and dispatches them to the appropriate MCP server.
func (b *Bridge) watchAndDispatch(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("creating fsnotify watcher: %w", err)
	}
	defer watcher.Close()

	if err := os.MkdirAll(b.ipcPath, 0o755); err != nil {
		return fmt.Errorf("creating IPC directory: %w", err)
	}

	if err := watcher.Add(b.ipcPath); err != nil {
		return fmt.Errorf("watching IPC directory: %w", err)
	}

	log.Printf("Watching %s for MCP requests", b.ipcPath)

	// Semaphore for concurrency control
	sem := make(chan struct{}, maxConcurrent)

	// Also watch for agent completion (result.json in parent /ipc/output/)
	outputDir := filepath.Join(filepath.Dir(b.ipcPath), "output")
	_ = watcher.Add(outputDir) // best-effort; dir may not exist yet

	var wg sync.WaitGroup

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return nil

		case event, ok := <-watcher.Events:
			if !ok {
				wg.Wait()
				return nil
			}

			filename := filepath.Base(event.Name)

			// Exit when agent completes
			if filename == "result.json" && filepath.Dir(event.Name) == outputDir {
				log.Printf("Agent completed (result.json detected), draining in-flight requests")
				wg.Wait()
				return nil
			}

			// Only process mcp-request-*.json files
			if !event.Has(fsnotify.Create) && !event.Has(fsnotify.Write) {
				continue
			}
			if !strings.HasPrefix(filename, "mcp-request-") || !strings.HasSuffix(filename, ".json") {
				continue
			}

			// Dedup: fsnotify fires both Create and Write
			if _, loaded := b.processed.LoadOrStore(event.Name, true); loaded {
				continue
			}

			// Acquire semaphore without blocking the event loop
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				wg.Wait()
				return nil
			}

			wg.Add(1)
			go func(path string) {
				defer wg.Done()
				defer func() { <-sem }() // release
				b.handleRequest(ctx, path)
				b.processed.Delete(path)
			}(event.Name)

		case err, ok := <-watcher.Errors:
			if !ok {
				wg.Wait()
				return nil
			}
			log.Printf("Watcher error: %v", err)
		}
	}
}

// extractIDFromFilename extracts the request ID from a filename like "mcp-request-<id>.json".
func extractIDFromFilename(path string) string {
	base := filepath.Base(path)
	base = strings.TrimPrefix(base, "mcp-request-")
	base = strings.TrimSuffix(base, ".json")
	return base
}

// handleRequest processes a single MCP request file.
func (b *Bridge) handleRequest(ctx context.Context, path string) {
	// Wait for background discovery to finish before touching clients/toolIndex.
	if !b.waitReady(ctx, 0) {
		return
	}

	// Small delay to ensure file write is complete
	time.Sleep(50 * time.Millisecond)

	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("Failed to read request %s: %v", filepath.Base(path), err)
		return
	}

	var req MCPRequest
	if err := json.Unmarshal(data, &req); err != nil {
		log.Printf("Failed to parse request %s: %v", filepath.Base(path), err)
		// Use ID from filename when JSON parse fails
		id := extractIDFromFilename(path)
		b.writeErrorResult(id, path, "invalid request JSON")
		mcpToolErrors.Add(ctx, 1, metric.WithAttributes(attribute.String("error", "invalid_json")))
		return
	}

	result, err := b.callTool(ctx, req.ID, req.Server, req.Tool, req.Arguments, req.Meta)
	if err != nil {
		log.Printf("MCP tool call failed: %v", err)
		b.writeErrorResult(req.ID, path, err.Error())
		return
	}

	b.writeResult(req.ID, path, result)
}

func (b *Bridge) callTool(ctx context.Context, requestID, requestedServer, requestedTool string, arguments json.RawMessage, metaStrings map[string]string) (*MCPResult, error) {
	start := time.Now()
	serverName := requestedServer
	toolName := requestedTool

	if serverName == "" {
		serverName, toolName = b.resolveByPrefix(requestedTool)
		if serverName == "" {
			mcpToolErrors.Add(ctx, 1, metric.WithAttributes(attribute.String("error", "no_server")))
			return nil, fmt.Errorf("no MCP server found for tool %q", requestedTool)
		}
	} else {
		_, toolName = b.resolveByPrefix(requestedTool)
		if toolName == requestedTool {
			toolName = requestedTool
		}
	}

	parentCtx := ctx
	if tp, ok := metaStrings["traceparent"]; ok {
		if remoteCtx := extractTraceparent(tp); remoteCtx.IsValid() {
			parentCtx = trace.ContextWithRemoteSpanContext(ctx, remoteCtx)
		}
	}

	ctx, span := bridgeTracer.Start(parentCtx, "mcp.tool_call",
		trace.WithAttributes(
			attribute.String("mcp.tool", toolName),
			attribute.String("mcp.server", serverName),
			attribute.String("mcp.request_id", requestID),
		),
	)
	defer span.End()

	attrs := metric.WithAttributes(
		attribute.String("mcp.server", serverName),
		attribute.String("mcp.tool", toolName),
	)
	mcpToolCalls.Add(ctx, 1, attrs)

	// Defense in depth: reject tool calls for tools not in the filtered index.
	if _, known := b.toolIndex[requestedTool]; !known {
		log.Printf("Tool %q not in filtered tool index, rejecting", requestedTool)
		span.SetStatus(codes.Error, "tool filtered")
		mcpToolErrors.Add(ctx, 1, attrs)
		return nil, fmt.Errorf("tool %q is not available (filtered)", requestedTool)
	}

	client, ok := b.clients[serverName]
	if !ok {
		log.Printf("No client for server %q", serverName)
		span.SetStatus(codes.Error, "server not connected")
		mcpToolErrors.Add(ctx, 1, attrs)
		return nil, fmt.Errorf("MCP server %q not connected", serverName)
	}

	// Build meta for trace propagation
	var meta map[string]any
	if len(metaStrings) > 0 {
		meta = make(map[string]any, len(metaStrings))
		for k, v := range metaStrings {
			meta[k] = v
		}
	}

	// Call the tool
	log.Printf("Calling MCP tool %q on server %q (request %s)", toolName, serverName, requestID)

	callResult, err := client.CallTool(ctx, toolName, arguments, meta)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "tool call failed")
		mcpToolErrors.Add(ctx, 1, attrs)
		return nil, err
	}

	// Build result
	result := MCPResult{
		ID:      requestID,
		Success: !callResult.IsError,
		IsError: callResult.IsError,
	}

	if callResult.IsError {
		// Extract error text from content
		for _, c := range callResult.Content {
			if c.Text != "" {
				result.Error = c.Text
				break
			}
		}
		span.SetStatus(codes.Error, "tool returned error")
		mcpToolErrors.Add(ctx, 1, attrs)
	}

	// Marshal content
	contentData, err := json.Marshal(callResult.Content)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "marshal failed")
		return nil, fmt.Errorf("failed to marshal tool result content")
	}
	result.Content = contentData

	mcpToolDuration.Record(ctx, float64(time.Since(start).Milliseconds()), attrs)
	return &result, nil
}

// resolveByPrefix finds the server for a prefixed tool name and returns
// the server name and the unprefixed tool name.
func (b *Bridge) resolveByPrefix(prefixedTool string) (serverName, toolName string) {
	// Check the exact tool index first
	if sn, ok := b.toolIndex[prefixedTool]; ok {
		// Strip prefix: find the prefix for this server and remove it + "_"
		for _, srv := range b.config.Servers {
			if srv.Name == sn && strings.HasPrefix(prefixedTool, srv.ToolsPrefix+"_") {
				return sn, strings.TrimPrefix(prefixedTool, srv.ToolsPrefix+"_")
			}
		}
		return sn, prefixedTool
	}

	// Fall back to prefix matching (for tools discovered after startup)
	for prefix, sn := range b.prefixIndex {
		if strings.HasPrefix(prefixedTool, prefix+"_") {
			return sn, strings.TrimPrefix(prefixedTool, prefix+"_")
		}
	}

	return "", prefixedTool
}

// writeResult writes an MCPResult to the result file.
func (b *Bridge) writeResult(id, reqPath string, result *MCPResult) {
	// Derive result path safely using filepath operations
	dir := filepath.Dir(reqPath)
	base := strings.Replace(filepath.Base(reqPath), "mcp-request-", "mcp-result-", 1)
	resPath := filepath.Join(dir, base)

	data, err := json.Marshal(result)
	if err != nil {
		log.Printf("Failed to marshal result for %s: %v", id, err)
		return
	}

	// Write atomically
	tmp := resPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Printf("Failed to write result for %s: %v", id, err)
		return
	}
	if err := os.Rename(tmp, resPath); err != nil {
		log.Printf("Failed to rename result for %s: %v", id, err)
		os.Remove(tmp)
		return
	}

	// Clean up request file
	if err := os.Remove(reqPath); err != nil && !os.IsNotExist(err) {
		log.Printf("Failed to clean up request file %s: %v", filepath.Base(reqPath), err)
	}
}

// writeErrorResult writes an error MCPResult.
func (b *Bridge) writeErrorResult(id, reqPath, errMsg string) {
	result := &MCPResult{
		ID:      id,
		Success: false,
		Error:   errMsg,
	}
	b.writeResult(id, reqPath, result)
}

// DiscoverAndWriteManifest runs only the discovery phase: connect to MCP servers,
// list tools, write the manifest, then return. Used by the init container.
func (b *Bridge) DiscoverAndWriteManifest(ctx context.Context) error {
	ctx, span := bridgeTracer.Start(ctx, "mcp-bridge.discover",
		trace.WithAttributes(attribute.String("agent_run_id", b.agentRunID)),
	)
	defer span.End()

	manifest, err := b.discoverTools(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "tool discovery failed")
		return err
	}

	span.SetAttributes(attribute.Int("mcp.tools_discovered", len(manifest.Tools)))
	b.manifest = manifest

	if err := WriteManifest(b.manifestPath, manifest); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "manifest write failed")
		return err
	}

	log.Printf("Wrote tool manifest with %d tools to %s", len(manifest.Tools), b.manifestPath)
	return nil
}

// extractTraceparent parses a W3C traceparent header into a SpanContext.
// Format: 00-<trace-id>-<span-id>-<flags>
func extractTraceparent(tp string) trace.SpanContext {
	parts := strings.Split(tp, "-")
	if len(parts) != 4 || parts[0] != "00" {
		return trace.SpanContext{}
	}

	traceID, err := trace.TraceIDFromHex(parts[1])
	if err != nil {
		return trace.SpanContext{}
	}

	spanID, err := trace.SpanIDFromHex(parts[2])
	if err != nil {
		return trace.SpanContext{}
	}

	var flags trace.TraceFlags
	if parts[3] == "01" {
		flags = trace.FlagsSampled
	}

	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: flags,
		Remote:     true,
	})
}
