package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sympozium-ai/sympozium/pkg/memoryclient"
)

// setMemoryClient swaps the package-level memoryClient for the test and
// returns a cleanup func. The previous client is restored on cleanup.
func setMemoryClient(t *testing.T, baseURL, ensemble, defaultVis string) func() {
	t.Helper()
	prevClient := memoryClient
	prevEnsemble := ensembleName
	prevVis := defaultStoreVisibility

	memoryClient = memoryclient.New(baseURL, memoryclient.WithTokenSource(memoryclient.StaticTokenSource("test-token")))
	ensembleName = ensemble
	defaultStoreVisibility = defaultVis

	return func() {
		memoryClient = prevClient
		ensembleName = prevEnsemble
		defaultStoreVisibility = prevVis
	}
}

func TestExecuteMemoryTool_UnknownReturnsError(t *testing.T) {
	defer setMemoryClient(t, "http://unused", "", "")()
	got := executeMemoryTool(context.Background(), "memory_bogus", `{}`)
	if !strings.HasPrefix(got, "Unknown memory tool") {
		t.Errorf("got %q, want 'Unknown memory tool' prefix", got)
	}
}

func TestExecuteMemoryTool_NoClientReturnsError(t *testing.T) {
	prev := memoryClient
	memoryClient = nil
	defer func() { memoryClient = prev }()

	got := executeMemoryTool(context.Background(), ToolMemorySearch, `{"query":"x"}`)
	if !strings.Contains(got, "MEMORY_SERVER_URL") {
		t.Errorf("got %q, want hint about MEMORY_SERVER_URL", got)
	}
}

func TestExecuteMemoryTool_RejectsBadScope(t *testing.T) {
	defer setMemoryClient(t, "http://unused", "", "")()
	got := executeMemoryTool(context.Background(), ToolMemorySearch, `{"query":"x","scope":"global"}`)
	if !strings.Contains(got, "scope must be") {
		t.Errorf("got %q, want scope validation error", got)
	}
}

func TestMemorySearch_AgentScopeOmitsEnsembleName(t *testing.T) {
	var got memoryclient.SearchRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"id":"1","content":"hello","tags":["t"]}]}`))
	}))
	defer srv.Close()
	defer setMemoryClient(t, srv.URL, "my-ensemble", "")()

	out := executeMemoryTool(context.Background(), ToolMemorySearch, `{"query":"kafka","top_k":7}`)

	if got.Scope != "agent" {
		t.Errorf("scope = %q, want agent", got.Scope)
	}
	if got.EnsembleName != "" {
		t.Errorf("ensembleName = %q, want empty for agent scope", got.EnsembleName)
	}
	if got.Query != "kafka" || got.TopK != 7 {
		t.Errorf("query/topK mismatch: %+v", got)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("formatted output missing content: %q", out)
	}
}

func TestMemorySearch_EnsembleScopePassesEnsembleName(t *testing.T) {
	var got memoryclient.SearchRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()
	defer setMemoryClient(t, srv.URL, "platform-team", "")()

	_ = executeMemoryTool(context.Background(), ToolMemorySearch, `{"query":"x","scope":"ensemble"}`)
	if got.Scope != "ensemble" || got.EnsembleName != "platform-team" {
		t.Errorf("scope=%q ensemble=%q, want ensemble/platform-team", got.Scope, got.EnsembleName)
	}
}

func TestMemoryStore_AgentScopeIgnoresVisibility(t *testing.T) {
	var got memoryclient.StoreRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"new-id"}`))
	}))
	defer srv.Close()
	defer setMemoryClient(t, srv.URL, "ens", "trusted")()

	out := executeMemoryTool(context.Background(), ToolMemoryStore, `{"content":"x","visibility":"public"}`)
	if got.Scope != "agent" {
		t.Errorf("scope = %q, want agent", got.Scope)
	}
	if got.Visibility != "" {
		t.Errorf("visibility = %q, want empty for agent scope", got.Visibility)
	}
	if !strings.Contains(out, "new-id") {
		t.Errorf("output missing id: %q", out)
	}
}

func TestMemoryStore_EnsembleScopeAppliesDefaultVisibility(t *testing.T) {
	var got memoryclient.StoreRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv.Close()
	defer setMemoryClient(t, srv.URL, "ens", "trusted")()

	_ = executeMemoryTool(context.Background(), ToolMemoryStore, `{"content":"x","scope":"ensemble"}`)
	if got.Visibility != "trusted" {
		t.Errorf("visibility = %q, want trusted (membrane default)", got.Visibility)
	}
	if got.EnsembleName != "ens" {
		t.Errorf("ensembleName = %q, want ens", got.EnsembleName)
	}
}

func TestMemoryStore_ExplicitVisibilityWins(t *testing.T) {
	var got memoryclient.StoreRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv.Close()
	defer setMemoryClient(t, srv.URL, "ens", "private")()

	_ = executeMemoryTool(context.Background(), ToolMemoryStore, `{"content":"x","scope":"ensemble","visibility":"public"}`)
	if got.Visibility != "public" {
		t.Errorf("visibility = %q, want explicit public", got.Visibility)
	}
}

func TestMemoryList_EnsembleScopeRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("scope") != "ensemble" || q.Get("ensembleName") != "team-a" || q.Get("limit") != "3" {
			t.Errorf("unexpected query: %v", q)
		}
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()
	defer setMemoryClient(t, srv.URL, "team-a", "")()

	_ = executeMemoryTool(context.Background(), ToolMemoryList, `{"scope":"ensemble","limit":3}`)
}

func TestQueryMemoryContext_TruncatesAndFormats(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"id":"a","content":"finding-1"},{"id":"b","content":"finding-2"}]}`))
	}))
	defer srv.Close()
	defer setMemoryClient(t, srv.URL, "", "")()

	got := queryMemoryContext("anything", 2)
	if !strings.Contains(got, "finding-1") || !strings.Contains(got, "finding-2") {
		t.Errorf("missing findings: %q", got)
	}
}

func TestQueryMemoryContext_ForbiddenSwallowedAsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	defer setMemoryClient(t, srv.URL, "", "")()

	if got := queryMemoryContext("x", 3); got != "" {
		t.Errorf("got %q, want empty on 403", got)
	}
}

func TestAutoStoreMemory_SendsAgentScope(t *testing.T) {
	var got memoryclient.StoreRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv.Close()
	defer setMemoryClient(t, srv.URL, "", "")()

	autoStoreMemory("a task", "a response")
	if got.Scope != "agent" {
		t.Errorf("scope = %q, want agent", got.Scope)
	}
	hasAuto, hasRun := false, false
	for _, tag := range got.Tags {
		if tag == "auto" {
			hasAuto = true
		}
		if tag == "agent-run" {
			hasRun = true
		}
	}
	if !hasAuto || !hasRun {
		t.Errorf("tags = %v, want [auto agent-run]", got.Tags)
	}
	if !strings.Contains(got.Content, "a task") || !strings.Contains(got.Content, "a response") {
		t.Errorf("content missing task/response: %q", got.Content)
	}
}
