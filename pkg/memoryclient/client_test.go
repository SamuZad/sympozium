package memoryclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileTokenSource_ReadsAndCaches(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("first-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ts := &FileTokenSource{Path: path, CacheFor: time.Hour}

	got, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "first-token" {
		t.Errorf("got %q, want first-token (trimmed)", got)
	}

	// Mutate the file. Because CacheFor is 1h, we expect the old value.
	if err := os.WriteFile(path, []byte("second-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = ts.Token(context.Background())
	if err != nil {
		t.Fatalf("Token (cached): %v", err)
	}
	if got != "first-token" {
		t.Errorf("cached token = %q, want first-token", got)
	}

	// Expire the cache and re-read.
	ts.expiresAt = time.Now().Add(-time.Second)
	got, err = ts.Token(context.Background())
	if err != nil {
		t.Fatalf("Token (re-read): %v", err)
	}
	if got != "second-token" {
		t.Errorf("re-read token = %q, want second-token", got)
	}
}

func TestFileTokenSource_MissingFile(t *testing.T) {
	ts := &FileTokenSource{Path: "/does/not/exist"}
	if _, err := ts.Token(context.Background()); err == nil {
		t.Fatal("expected error for missing token file")
	}
}

func TestFileTokenSource_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ts := &FileTokenSource{Path: path}
	if _, err := ts.Token(context.Background()); err == nil {
		t.Fatal("expected error for empty token file")
	}
}

func TestClient_Store_SendsBearerAndBody(t *testing.T) {
	var gotAuth, gotPath, gotCT string
	var gotBody StoreRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Entry{ID: "abc123"})
	}))
	defer srv.Close()

	c := New(srv.URL, WithTokenSource(StaticTokenSource("test-token")))
	got, err := c.Store(context.Background(), StoreRequest{
		Scope:        "ensemble",
		EnsembleName: "platform",
		AgentName:    "sre",
		Content:      "seed text",
		Tags:         []string{"seed", "v1"},
	})
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if got == nil || got.ID != "abc123" {
		t.Errorf("Store returned %+v, want id=abc123", got)
	}
	if gotPath != "/v1/store" {
		t.Errorf("path = %q, want /v1/store", gotPath)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want Bearer test-token", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if gotBody.Scope != "ensemble" || gotBody.EnsembleName != "platform" ||
		gotBody.AgentName != "sre" || gotBody.Content != "seed text" ||
		strings.Join(gotBody.Tags, ",") != "seed,v1" {
		t.Errorf("body mismatch: %+v", gotBody)
	}
}

func TestClient_Store_PropagatesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"not a member"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, WithTokenSource(StaticTokenSource("t")))
	_, err := c.Store(context.Background(), StoreRequest{Scope: "agent", Content: "x"})
	if err == nil {
		t.Fatal("expected error")
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error %T, want *HTTPError", err)
	}
	if httpErr.Status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", httpErr.Status)
	}
}

func TestClient_Store_RejectsEmptyBaseURL(t *testing.T) {
	c := New("", WithTokenSource(StaticTokenSource("t")))
	if _, err := c.Store(context.Background(), StoreRequest{Scope: "agent", Content: "x"}); err == nil {
		t.Fatal("expected error for empty BaseURL")
	}
}

func TestClient_Store_NilClient(t *testing.T) {
	var c *Client
	if _, err := c.Store(context.Background(), StoreRequest{}); err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestClient_DeleteByTags_SendsBodyAndAuth(t *testing.T) {
	var gotAuth, gotPath, gotMethod string
	var gotBody DeleteByTagsRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"deleted":3}`))
	}))
	defer srv.Close()

	c := New(srv.URL, WithTokenSource(StaticTokenSource("admin-token")))
	n, err := c.DeleteByTags(context.Background(), DeleteByTagsRequest{
		Namespace:   "ns",
		Scope:       "agent",
		AgentName:   "alice",
		RequireTags: []string{"seed", "seed-hash:abc"},
	})
	if err != nil {
		t.Fatalf("DeleteByTags: %v", err)
	}
	if n != 3 {
		t.Errorf("deleted = %d, want 3", n)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/admin/delete-by-tags" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer admin-token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotBody.Scope != "agent" || gotBody.AgentName != "alice" ||
		strings.Join(gotBody.RequireTags, ",") != "seed,seed-hash:abc" {
		t.Errorf("body mismatch: %+v", gotBody)
	}
}

func TestClient_DeleteByTags_RejectsEmptyTags(t *testing.T) {
	c := New("http://unused", WithTokenSource(StaticTokenSource("t")))
	if _, err := c.DeleteByTags(context.Background(), DeleteByTagsRequest{Scope: "agent", AgentName: "x"}); err == nil {
		t.Fatal("expected error for empty RequireTags")
	}
}

func TestClient_DeleteByTags_PropagatesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"admin only"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, WithTokenSource(StaticTokenSource("t")))
	_, err := c.DeleteByTags(context.Background(), DeleteByTagsRequest{
		Scope:       "agent",
		AgentName:   "x",
		RequireTags: []string{"seed"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusForbidden {
		t.Errorf("got %v, want HTTPError 403", err)
	}
}

func TestClient_Search_PostsBodyAndDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/search" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer t" {
			t.Errorf("auth = %q, want Bearer t", got)
		}
		var req SearchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Scope != "agent" || req.AgentName != "alice" || req.Query != "kafka lag" || req.TopK != 3 {
			t.Errorf("unexpected req: %+v", req)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"id":"1","content":"foo","tags":["x"]}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, WithTokenSource(StaticTokenSource("t")))
	hits, err := c.Search(context.Background(), SearchRequest{
		Scope:     "agent",
		AgentName: "alice",
		Query:     "kafka lag",
		TopK:      3,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "1" || hits[0].Content != "foo" {
		t.Errorf("unexpected hits: %+v", hits)
	}
}

func TestClient_List_EncodesQueryParams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/list" || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("scope") != "ensemble" || q.Get("ensembleName") != "platform" || q.Get("limit") != "5" {
			t.Errorf("unexpected query: %v", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, WithTokenSource(StaticTokenSource("t")))
	if _, err := c.List(context.Background(), ListRequest{
		Scope:        "ensemble",
		EnsembleName: "platform",
		Limit:        5,
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
}

func TestClient_Search_PropagatesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := New(srv.URL, WithTokenSource(StaticTokenSource("t")))
	_, err := c.Search(context.Background(), SearchRequest{Scope: "agent", Query: "x"})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusForbidden {
		t.Errorf("got %v, want HTTPError 403", err)
	}
}
