// Package memoryclient is a minimal HTTP client for the Sympozium central
// memory-server. It is consumed by the controller (to write ensemble seed
// memories) and is intended to be reused by the apiserver and other in-cluster
// components that need read/write access on behalf of a Kubernetes identity.
//
// The agent-runner has its own (legacy) client for now; the planned refactor
// will migrate it onto this package.
package memoryclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultProjectedTokenPath is where Kubernetes mounts the projected
// ServiceAccount token by default. The kubelet refreshes this file as
// the token approaches expiry, so re-reading on demand is sufficient.
const DefaultProjectedTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// TokenSource supplies a bearer token for a single request. Implementations
// must be safe for concurrent use.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// StaticTokenSource always returns the same token. Intended for tests.
type StaticTokenSource string

func (s StaticTokenSource) Token(_ context.Context) (string, error) { return string(s), nil }

// FileTokenSource reads a bearer token from disk on each call, caching it
// for a short window to avoid hammering the filesystem. The kubelet rotates
// the projected SA token file in-place, so re-reading picks up the new
// value without process restarts.
type FileTokenSource struct {
	Path string
	// CacheFor controls how long a successfully read token is reused
	// before the next disk read. Zero means "always re-read".
	CacheFor time.Duration

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// NewProjectedSATokenSource returns a FileTokenSource backed by the default
// projected ServiceAccount token path, cached for 60 seconds. The kubelet
// rotates the token well before its TTL, so a short cache is safe and cheap.
func NewProjectedSATokenSource() *FileTokenSource {
	return &FileTokenSource{Path: DefaultProjectedTokenPath, CacheFor: 60 * time.Second}
}

func (f *FileTokenSource) Token(_ context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.token != "" && time.Now().Before(f.expiresAt) {
		return f.token, nil
	}
	data, err := os.ReadFile(f.Path)
	if err != nil {
		return "", fmt.Errorf("memoryclient: read token %q: %w", f.Path, err)
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", fmt.Errorf("memoryclient: empty token at %q", f.Path)
	}
	f.token = tok
	if f.CacheFor > 0 {
		f.expiresAt = time.Now().Add(f.CacheFor)
	}
	return tok, nil
}

// Client is the HTTP client for the memory-server v1 API.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	Tokens     TokenSource
}

// Option mutates a Client during construction.
type Option func(*Client)

// WithHTTPClient overrides the default HTTP client.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.HTTPClient = h } }

// WithTokenSource overrides the default in-cluster projected SA token source.
func WithTokenSource(ts TokenSource) Option { return func(c *Client) { c.Tokens = ts } }

// New constructs a Client. The baseURL should be the in-cluster service URL
// of the memory-server (e.g. "http://release-memory-server.sympozium-system.svc:8080").
// If no TokenSource is supplied, the default projected SA token is used.
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
		Tokens:     NewProjectedSATokenSource(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// StoreRequest mirrors cmd/memory-server's storeRequest. See that file for
// the canonical schema; we keep this in sync by hand because the server is
// in this repo and breaking changes will surface in compile-time references
// from the controller.
type StoreRequest struct {
	Scope        string   `json:"scope"` // "agent" or "ensemble"
	AgentName    string   `json:"agentName,omitempty"`
	EnsembleName string   `json:"ensembleName,omitempty"`
	Content      string   `json:"content"`
	Tags         []string `json:"tags,omitempty"`
	Visibility   string   `json:"visibility,omitempty"`
	ParentID     string   `json:"parentId,omitempty"`
	TTLDays      int      `json:"ttlDays,omitempty"`
}

// Entry is the server's stored-memory response shape. We keep this loose
// (string ID, generic timestamp) so server-side schema tweaks don't ripple
// into every caller.
type Entry struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt,omitempty"`
}

// Store posts a new memory entry. Returns the server-assigned Entry on success.
func (c *Client) Store(ctx context.Context, req StoreRequest) (*Entry, error) {
	if c == nil {
		return nil, errors.New("memoryclient: nil client")
	}
	if c.BaseURL == "" {
		return nil, errors.New("memoryclient: empty BaseURL")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("memoryclient: marshal store request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/store", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("memoryclient: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if c.Tokens != nil {
		tok, terr := c.Tokens.Token(ctx)
		if terr != nil {
			return nil, terr
		}
		httpReq.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("memoryclient: store: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode >= 400 {
		return nil, &HTTPError{Status: resp.StatusCode, Body: string(respBody)}
	}

	var entry Entry
	if err := json.Unmarshal(respBody, &entry); err != nil {
		// Server returned 2xx but unparsable body — still treat as success
		// so we don't retry a write that succeeded.
		return &Entry{}, nil
	}
	return &entry, nil
}

// HTTPError is returned for non-2xx responses from the memory-server. The
// status code lets callers distinguish recoverable errors (5xx) from
// permanent ones (4xx) without parsing the body.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("memoryclient: server returned %d: %s", e.Status, e.Body)
}

// DeleteByTagsRequest mirrors the server's deleteByTagsRequest. RequireTags
// must be non-empty: the server refuses to interpret an empty list as a
// full-scope wipe, and so do we.
type DeleteByTagsRequest struct {
	Namespace    string   `json:"namespace,omitempty"`
	Scope        string   `json:"scope"`
	AgentName    string   `json:"agentName,omitempty"`
	EnsembleName string   `json:"ensembleName,omitempty"`
	RequireTags  []string `json:"requireTags"`
}

// DeleteByTagsResponse is the server's success envelope.
type DeleteByTagsResponse struct {
	Deleted int64 `json:"deleted"`
}

// DeleteByTags removes every memory in the chosen scope whose tag set is a
// superset of req.RequireTags. The caller must be an admin (its SA must be
// in MEMORY_ADMIN_SAS). Returns the number of rows deleted.
func (c *Client) DeleteByTags(ctx context.Context, req DeleteByTagsRequest) (int64, error) {
	if c == nil {
		return 0, errors.New("memoryclient: nil client")
	}
	if c.BaseURL == "" {
		return 0, errors.New("memoryclient: empty BaseURL")
	}
	if len(req.RequireTags) == 0 {
		return 0, errors.New("memoryclient: DeleteByTags requires non-empty RequireTags")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return 0, fmt.Errorf("memoryclient: marshal delete request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/admin/delete-by-tags", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("memoryclient: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if c.Tokens != nil {
		tok, terr := c.Tokens.Token(ctx)
		if terr != nil {
			return 0, terr
		}
		httpReq.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return 0, fmt.Errorf("memoryclient: delete-by-tags: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode >= 400 {
		return 0, &HTTPError{Status: resp.StatusCode, Body: string(respBody)}
	}

	var out DeleteByTagsResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return 0, nil
	}
	return out.Deleted, nil
}

// SearchRequest mirrors cmd/memory-server's searchRequest.
type SearchRequest struct {
	Scope        string   `json:"scope"`
	AgentName    string   `json:"agentName,omitempty"`
	EnsembleName string   `json:"ensembleName,omitempty"`
	Query        string   `json:"query"`
	TopK         int      `json:"topK,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	MaxAgeHours  int      `json:"maxAgeHours,omitempty"`
}

// SearchHit is the per-row payload returned by /v1/search. We mirror the
// fields the agent-runner renders for the LLM; unknown fields are ignored.
type SearchHit struct {
	ID         string    `json:"id"`
	Content    string    `json:"content"`
	Tags       []string  `json:"tags,omitempty"`
	Visibility string    `json:"visibility,omitempty"`
	Score      float64   `json:"score,omitempty"`
	CreatedAt  time.Time `json:"createdAt,omitempty"`
}

type searchResponse struct {
	Results []SearchHit `json:"results"`
}

// Search performs a hybrid search against the memory-server and returns the
// hits the caller is authorised to see.
func (c *Client) Search(ctx context.Context, req SearchRequest) ([]SearchHit, error) {
	var out searchResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/search", req, &out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

// List returns the most recent entries the caller is authorised to see.
// Limit/Offset map to standard pagination semantics. Tag filtering is not
// supported by the server's list endpoint — use Search for tag-narrowed reads.
type ListRequest struct {
	// Namespace is honoured only when the caller is an admin SA. For
	// non-admin tokens the server pins the namespace to the caller's own.
	Namespace    string
	Scope        string
	AgentName    string
	EnsembleName string
	Limit        int
	Offset       int
}

func (c *Client) List(ctx context.Context, req ListRequest) ([]SearchHit, error) {
	if c == nil {
		return nil, errors.New("memoryclient: nil client")
	}
	if c.BaseURL == "" {
		return nil, errors.New("memoryclient: empty BaseURL")
	}
	q := url.Values{}
	if req.Namespace != "" {
		q.Set("namespace", req.Namespace)
	}
	if req.Scope != "" {
		q.Set("scope", req.Scope)
	}
	if req.AgentName != "" {
		q.Set("agentName", req.AgentName)
	}
	if req.EnsembleName != "" {
		q.Set("ensembleName", req.EnsembleName)
	}
	if req.Limit > 0 {
		q.Set("limit", strconv.Itoa(req.Limit))
	}
	if req.Offset > 0 {
		q.Set("offset", strconv.Itoa(req.Offset))
	}
	path := "/v1/list"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	var out searchResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

// DeleteScopeRequest targets every memory in a single scope. The caller
// must be an admin SA (its identity must be in MEMORY_ADMIN_SAS).
type DeleteScopeRequest struct {
	Namespace    string
	Scope        string // "agent" or "ensemble"
	AgentName    string
	EnsembleName string
}

// DeleteScope wipes every row in the chosen scope. Returns the number of
// rows deleted. Admin-only; non-admin callers get 403 from the server.
func (c *Client) DeleteScope(ctx context.Context, req DeleteScopeRequest) (int64, error) {
	if c == nil {
		return 0, errors.New("memoryclient: nil client")
	}
	if c.BaseURL == "" {
		return 0, errors.New("memoryclient: empty BaseURL")
	}
	q := url.Values{}
	if req.Namespace != "" {
		q.Set("namespace", req.Namespace)
	}
	if req.Scope != "" {
		q.Set("scope", req.Scope)
	}
	if req.AgentName != "" {
		q.Set("agentName", req.AgentName)
	}
	if req.EnsembleName != "" {
		q.Set("ensembleName", req.EnsembleName)
	}
	path := "/v1/admin/scope"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	var out DeleteByTagsResponse // server returns {"deleted": N}
	if err := c.doJSON(ctx, http.MethodDelete, path, nil, &out); err != nil {
		return 0, err
	}
	return out.Deleted, nil
}

// doJSON is the shared request/response codec for the typed methods above.
// body is JSON-encoded into the request when non-nil; result is JSON-decoded
// from the response when non-nil. 2xx with empty body is treated as success.
func (c *Client) doJSON(ctx context.Context, method, path string, body, result any) error {
	if c == nil {
		return errors.New("memoryclient: nil client")
	}
	if c.BaseURL == "" {
		return errors.New("memoryclient: empty BaseURL")
	}
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("memoryclient: marshal %s %s: %w", method, path, err)
		}
		reqBody = bytes.NewReader(buf)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("memoryclient: build request: %w", err)
	}
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	httpReq.Header.Set("Accept", "application/json")
	if c.Tokens != nil {
		tok, terr := c.Tokens.Token(ctx)
		if terr != nil {
			return terr
		}
		httpReq.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("memoryclient: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return &HTTPError{Status: resp.StatusCode, Body: string(respBody)}
	}
	if result == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, result); err != nil {
		return fmt.Errorf("memoryclient: decode response: %w", err)
	}
	return nil
}
