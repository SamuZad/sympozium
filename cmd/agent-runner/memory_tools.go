package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sympozium-ai/sympozium/pkg/memoryclient"
)

// Memory tool names. These map onto the central memory-server v1 API.
// A single set of tools targets either the agent's private scope (default)
// or the ensemble shared scope via the `scope` argument; the server enforces
// membership and visibility based on the caller's SA bearer token.
const (
	ToolMemorySearch = "memory_search"
	ToolMemoryStore  = "memory_store"
	ToolMemoryList   = "memory_list"
)

var memoryToolNames = map[string]bool{
	ToolMemorySearch: true,
	ToolMemoryStore:  true,
	ToolMemoryList:   true,
}

func isMemoryTool(name string) bool { return memoryToolNames[name] }

// memoryClient is the shared HTTP client to the central memory-server.
// nil when MEMORY_SERVER_URL is unset (memory disabled for this run).
var memoryClient *memoryclient.Client

// ensembleName, when non-empty, lets the agent target the ensemble shared
// scope without specifying agentName in store/search requests.
var ensembleName string

// defaultStoreVisibility comes from the membrane (WORKFLOW_MEMBRANE_VISIBILITY)
// and applies to ensemble-scope writes when the LLM does not pass one explicitly.
// Empty defaults to the server's own default (currently "public").
var defaultStoreVisibility string

// initMemoryTools wires up the memory tools when MEMORY_SERVER_URL is set.
// It returns the tool definitions to advertise to the LLM, or nil when
// memory is disabled for this run.
func initMemoryTools() []ToolDef {
	url := strings.TrimRight(os.Getenv("MEMORY_SERVER_URL"), "/")
	if url == "" {
		return nil
	}
	memoryClient = memoryclient.New(url)
	ensembleName = os.Getenv("ENSEMBLE_NAME")
	defaultStoreVisibility = os.Getenv("WORKFLOW_MEMBRANE_VISIBILITY")

	log.Printf("memory tools wired: service=%s ensemble=%q default_visibility=%q",
		url, ensembleName, defaultStoreVisibility)
	return memoryToolDefs()
}

func memoryToolDefs() []ToolDef {
	scopeProp := map[string]any{
		"type":        "string",
		"enum":        []string{"agent", "ensemble"},
		"description": "Which scope to target. 'agent' (default) is your private memory; 'ensemble' is shared across personas in the same ensemble.",
	}

	defs := []ToolDef{
		{
			Name:        ToolMemorySearch,
			Description: "Search persistent memory for relevant past findings, investigations, and shared knowledge. Use this before starting any investigation.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Natural language search query.",
					},
					"top_k": map[string]any{
						"type":        "integer",
						"description": "Maximum number of results (default 5).",
					},
					"scope": scopeProp,
				},
				"required": []string{"query"},
			},
		},
		{
			Name:        ToolMemoryStore,
			Description: "Store a finding, investigation result, or important context for future runs. Be specific: include root cause, resolution steps, service names, namespaces.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content": map[string]any{
						"type":        "string",
						"description": "The content to store.",
					},
					"tags": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Categorisation tags (e.g. ['kafka','consumer-lag']).",
					},
					"scope": scopeProp,
					"visibility": map[string]any{
						"type":        "string",
						"enum":        []string{"public", "trusted"},
						"description": "Optional visibility on ensemble-scope writes (public = anyone in the ensemble; trusted = ensemble members on your agent's trust list). 'private' is not allowed on ensemble scope — use scope=agent for personal notes.",
					},
				},
				"required": []string{"content"},
			},
		},
		{
			Name:        ToolMemoryList,
			Description: "List the most recent entries you can see in the chosen scope.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"scope": scopeProp,
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum entries (default 20).",
					},
				},
			},
		},
	}
	return defs
}

// executeMemoryTool dispatches a memory tool call. Returns the formatted
// string the LLM should see; errors are returned as plain strings prefixed
// with "Error:" so the model can recover.
func executeMemoryTool(ctx context.Context, name, argsJSON string) string {
	if memoryClient == nil {
		return "Error: memory server not configured (MEMORY_SERVER_URL not set)"
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %v", err)
	}

	scope := stringArg(args, "scope", "agent")
	if scope != "agent" && scope != "ensemble" {
		return fmt.Sprintf("Error: scope must be 'agent' or 'ensemble', got %q", scope)
	}

	switch name {
	case ToolMemorySearch:
		return runMemorySearch(ctx, scope, args)
	case ToolMemoryStore:
		return runMemoryStore(ctx, scope, args)
	case ToolMemoryList:
		return runMemoryList(ctx, scope, args)
	default:
		return fmt.Sprintf("Unknown memory tool: %s", name)
	}
}

func runMemorySearch(ctx context.Context, scope string, args map[string]any) string {
	query := stringArg(args, "query", "")
	if query == "" {
		return "Error: query is required"
	}
	topK := intArg(args, "top_k", 5)
	req := memoryclient.SearchRequest{
		Scope: scope,
		Query: query,
		TopK:  topK,
	}
	if scope == "ensemble" {
		req.EnsembleName = ensembleName
	}
	hits, err := memoryClient.Search(ctx, req)
	if err != nil {
		return fmt.Sprintf("Memory search error: %v", err)
	}
	return formatHits(hits)
}

func runMemoryStore(ctx context.Context, scope string, args map[string]any) string {
	content := stringArg(args, "content", "")
	if content == "" {
		return "Error: content is required"
	}
	tags := stringSliceArg(args, "tags")
	req := memoryclient.StoreRequest{
		Scope:   scope,
		Content: content,
		Tags:    tags,
	}
	if scope == "ensemble" {
		req.EnsembleName = ensembleName
		req.Visibility = stringArg(args, "visibility", defaultStoreVisibility)
	}
	entry, err := memoryClient.Store(ctx, req)
	if err != nil {
		return fmt.Sprintf("Memory store error: %v", err)
	}
	if entry != nil && entry.ID != "" {
		return fmt.Sprintf("Stored memory %s", entry.ID)
	}
	return "Stored memory"
}

func runMemoryList(ctx context.Context, scope string, args map[string]any) string {
	limit := intArg(args, "limit", 20)
	req := memoryclient.ListRequest{
		Scope: scope,
		Limit: limit,
	}
	if scope == "ensemble" {
		req.EnsembleName = ensembleName
	}
	hits, err := memoryClient.List(ctx, req)
	if err != nil {
		return fmt.Sprintf("Memory list error: %v", err)
	}
	return formatHits(hits)
}

// memoryContextMaxChars caps auto-injected memory context so it does not
// blow out the prompt budget. ~2000 chars ≈ 500 tokens.
const memoryContextMaxChars = 2000

// queryMemoryContext fetches a small bundle of relevant memories for the
// current task and returns a pre-formatted block for injection into the
// system prompt. Empty on any error or no results.
func queryMemoryContext(task string, maxResults int) string {
	if memoryClient == nil {
		return ""
	}
	query := task
	if len(query) > 200 {
		query = query[:200]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	hits, err := memoryClient.Search(ctx, memoryclient.SearchRequest{
		Scope: "agent",
		Query: query,
		TopK:  maxResults,
	})
	if err != nil {
		var herr *memoryclient.HTTPError
		if errors.As(err, &herr) && herr.Status == http.StatusForbidden {
			// First-run agents have no membership row; treat as no context.
			return ""
		}
		log.Printf("memory context: search failed: %v", err)
		return ""
	}
	if len(hits) == 0 {
		return ""
	}
	formatted := formatHits(hits)
	if len(formatted) > memoryContextMaxChars {
		cut := strings.LastIndex(formatted[:memoryContextMaxChars], "\n---\n")
		if cut > 0 {
			formatted = formatted[:cut]
		} else {
			formatted = formatted[:memoryContextMaxChars]
		}
	}
	return formatted
}

// autoStoreMemory persists a short task/response breadcrumb so future runs
// for this agent have at least some recall. Fire-and-forget.
func autoStoreMemory(task, response string) {
	if memoryClient == nil {
		return
	}
	const maxTask, maxResponse = 500, 1000
	if len(task) > maxTask {
		task = task[:maxTask] + "..."
	}
	if len(response) > maxResponse {
		response = response[:maxResponse] + "..."
	}
	content := fmt.Sprintf("Task: %s\n\nResponse: %s", task, response)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := memoryClient.Store(ctx, memoryclient.StoreRequest{
		Scope:   "agent",
		Content: content,
		Tags:    []string{"auto", "agent-run"},
	}); err != nil {
		log.Printf("auto-store memory failed: %v", err)
	}
}

// formatHits renders a slice of SearchHits for the LLM. Single source of
// truth across search/list responses so output is consistent.
func formatHits(hits []memoryclient.SearchHit) string {
	if len(hits) == 0 {
		return "(no results)"
	}
	var sb strings.Builder
	for i, h := range hits {
		if i > 0 {
			sb.WriteString("\n---\n")
		}
		if h.ID != "" {
			sb.WriteString(fmt.Sprintf("**Memory %s**", h.ID))
		}
		if !h.CreatedAt.IsZero() {
			sb.WriteString(fmt.Sprintf(" (%s)", h.CreatedAt.Format(time.RFC3339)))
		}
		if len(h.Tags) > 0 {
			sb.WriteString(fmt.Sprintf(" [%s]", strings.Join(h.Tags, ", ")))
		}
		sb.WriteString("\n")
		sb.WriteString(h.Content)
		sb.WriteString("\n")
	}
	return sb.String()
}

// --- argument helpers ---

func stringArg(args map[string]any, key, fallback string) string {
	if v, ok := args[key].(string); ok && v != "" {
		return v
	}
	return fallback
}

func intArg(args map[string]any, key string, fallback int) int {
	switch v := args[key].(type) {
	case float64:
		if v > 0 {
			return int(v)
		}
	case int:
		if v > 0 {
			return v
		}
	}
	return fallback
}

func stringSliceArg(args map[string]any, key string) []string {
	raw, ok := args[key].([]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}
