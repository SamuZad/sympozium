package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// server bundles the HTTP handlers with their dependencies.
type server struct {
	cfg        *Config
	store      *store
	embedder   Embedder
	auth       *authenticator
	membership *membershipResolver
}

// runServe starts the HTTP server. It refuses to start if the schema is
// missing — the migration Job must have run first.
func runServe(ctx context.Context, cfg *Config) error {
	pool, err := newPool(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	if err := verifySchema(ctx, pool, cfg.Embedding.Dimension); err != nil {
		return fmt.Errorf("schema check (did the migration Job run?): %w", err)
	}

	embedder, err := NewEmbedder(cfg.Embedding)
	if err != nil {
		return err
	}

	auth, err := newAuthenticator(newK8sClient, cfg.TokenCacheSize, cfg.TokenCacheTTL, cfg.AdminServiceAccounts)
	if err != nil {
		return err
	}
	ctrlC, err := newCtrlClient()
	if err != nil {
		return err
	}
	mem, err := newMembershipResolver(ctrlC, cfg.MembershipCacheTTL, cfg.MembershipCacheSize)
	if err != nil {
		return err
	}

	s := &server{
		cfg:        cfg,
		store:      newStore(pool, cfg.Embedding.Dimension, cfg.DefaultTTLDays),
		embedder:   embedder,
		auth:       auth,
		membership: mem,
	}

	mux := http.NewServeMux()
	// Health endpoints are unauthenticated so kubelet probes work.
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz(pool))

	// Authenticated routes.
	protected := http.NewServeMux()
	protected.HandleFunc("POST /v1/store", s.handleStore)
	protected.HandleFunc("POST /v1/search", s.handleSearch)
	protected.HandleFunc("GET /v1/list", s.handleList)
	protected.HandleFunc("GET /v1/stats", s.handleStats)
	protected.HandleFunc("GET /v1/provenance", s.handleProvenance)
	protected.HandleFunc("DELETE /v1/admin/scope", s.handleAdminDeleteScope)
	protected.HandleFunc("POST /v1/admin/delete-by-tags", s.handleAdminDeleteByTags)
	mux.Handle("/v1/", auth.middleware(protected))

	// Background pruner.
	go s.prunerLoop(ctx)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("memory-server listening", "addr", cfg.Listen, "embedding_model", cfg.Embedding.Model, "embedding_dim", cfg.Embedding.Dimension)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

func verifySchema(ctx context.Context, pool *pgxpool.Pool, wantDim int) error {
	var ok bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'memories')`,
	).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("table 'memories' does not exist")
	}
	// Confirm the embedding column's declared vector(N) width matches the
	// configured dimension. pgvector encodes N in pg_attribute.atttypmod
	// (no offset; the raw value is the dimension). Mismatch means the
	// migration ran against a different MEMORY_EMBEDDING_DIM than the
	// running server now sees — every write would fail at Postgres with a
	// cryptic "expected N dimensions" error, so we fail fast instead.
	var colDim int
	err = pool.QueryRow(ctx, `
		SELECT a.atttypmod
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		WHERE c.relname = 'memories' AND a.attname = 'embedding'`,
	).Scan(&colDim)
	if err != nil {
		return fmt.Errorf("read embedding column dimension: %w", err)
	}
	if colDim != wantDim {
		return fmt.Errorf("embedding column is vector(%d) but MEMORY_EMBEDDING_DIM=%d; re-run `memory-server migrate` against a fresh database, or change the env var to match", colDim, wantDim)
	}
	return nil
}

func (s *server) prunerLoop(ctx context.Context) {
	t := time.NewTicker(15 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := s.store.pruneExpired(ctx)
			if err != nil {
				slog.Warn("prune failed", "err", err)
				continue
			}
			if n > 0 {
				slog.Info("pruned expired memories", "deleted", n)
			}
		}
	}
}

// ------------------ Handlers ------------------

func (s *server) healthz(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("ok"))
}

func (s *server) readyz(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			http.Error(w, "not ready: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready"))
	}
}

type storeRequest struct {
	Scope        string   `json:"scope"`
	AgentName    string   `json:"agentName,omitempty"`
	EnsembleName string   `json:"ensembleName,omitempty"`
	Content      string   `json:"content"`
	Tags         []string `json:"tags,omitempty"`
	Visibility   string   `json:"visibility,omitempty"`
	ParentID     string   `json:"parentId,omitempty"`
	TTLDays      int      `json:"ttlDays,omitempty"`
}

func (s *server) handleStore(w http.ResponseWriter, r *http.Request) {
	id, ok := identityFromCtx(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no identity")
		return
	}
	var req storeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if req.Content == "" {
		writeErr(w, http.StatusBadRequest, "content required")
		return
	}

	m, err := s.membership.Resolve(r.Context(), id)
	if err != nil && !id.IsAdmin {
		writeErr(w, http.StatusForbidden, "membership: "+err.Error())
		return
	}

	// Authorization: caller may only write into a scope they belong to.
	switch req.Scope {
	case "agent":
		target := req.AgentName
		if target == "" {
			target = m.AgentName
			req.AgentName = target
		}
		if !id.IsAdmin && target != m.AgentName {
			writeErr(w, http.StatusForbidden, "cannot write into another agent's private scope")
			return
		}
	case "ensemble":
		if req.EnsembleName == "" {
			req.EnsembleName = m.EnsembleName
		}
		if !id.IsAdmin && (req.EnsembleName == "" || req.EnsembleName != m.EnsembleName) {
			writeErr(w, http.StatusForbidden, "caller is not a member of ensemble "+req.EnsembleName)
			return
		}
		if req.AgentName == "" {
			req.AgentName = m.AgentName
		}
		// Ensemble scope is for sharing; private rows wouldn't be readable
		// by anyone (handleSearch/handleList only allow public+trusted for
		// ensemble scope). Reject instead of accepting a write-only black
		// hole. Agents that want a personal note should use scope=agent.
		if req.Visibility == "private" {
			writeErr(w, http.StatusBadRequest, "visibility 'private' is not allowed on ensemble-scope writes; use scope=agent for personal notes")
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "scope must be 'agent' or 'ensemble'")
		return
	}

	embCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	vec, err := s.embedder.Embed(embCtx, req.Content)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "embed: "+err.Error())
		return
	}

	row, err := s.store.insert(r.Context(), storeArgs{
		Namespace:      id.Namespace,
		Scope:          req.Scope,
		AgentName:      req.AgentName,
		EnsembleName:   req.EnsembleName,
		SourceAgent:    m.AgentName,
		Content:        req.Content,
		Embedding:      vec,
		EmbeddingModel: s.embedder.Model(),
		Tags:           req.Tags,
		Visibility:     req.Visibility,
		ParentID:       req.ParentID,
		TTLDays:        req.TTLDays,
	})
	if err != nil {
		switch {
		case errors.Is(err, errPoolModel):
			writeErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, errDimMismatch):
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
		case errors.Is(err, errScopeInvalid):
			writeErr(w, http.StatusBadRequest, err.Error())
		default:
			slog.Error("store insert", "err", err)
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	writeJSON(w, http.StatusCreated, row)
}

type searchRequest struct {
	Scope        string   `json:"scope"`
	AgentName    string   `json:"agentName,omitempty"`
	EnsembleName string   `json:"ensembleName,omitempty"`
	Query        string   `json:"query"`
	TopK         int      `json:"topK,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	MaxAgeHours  int      `json:"maxAgeHours,omitempty"`
}

func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	id, ok := identityFromCtx(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no identity")
		return
	}
	var req searchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if req.Query == "" {
		writeErr(w, http.StatusBadRequest, "query required")
		return
	}

	m, err := s.membership.Resolve(r.Context(), id)
	if err != nil && !id.IsAdmin {
		writeErr(w, http.StatusForbidden, "membership: "+err.Error())
		return
	}

	switch req.Scope {
	case "agent":
		if req.AgentName == "" {
			req.AgentName = m.AgentName
		}
	case "ensemble":
		if req.EnsembleName == "" {
			req.EnsembleName = m.EnsembleName
		}
		if !id.IsAdmin && req.EnsembleName != m.EnsembleName {
			writeErr(w, http.StatusForbidden, "caller is not a member of ensemble "+req.EnsembleName)
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "scope must be 'agent' or 'ensemble'")
		return
	}

	embCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	qv, err := s.embedder.Embed(embCtx, req.Query)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "embed: "+err.Error())
		return
	}

	var allowVis []string
	var reachable []reachablePeer
	switch {
	case id.IsAdmin:
		allowVis = []string{"public", "trusted", "private"}
	case req.Scope == "agent":
		allowVis = visibilitiesFor(m.AgentName, req.AgentName, m.TrustPeers)
	case req.Scope == "ensemble":
		// Within a pool every member can see public + trusted; private
		// rows are visible only to the writer.
		allowVis = []string{"public", "trusted"}
		if req.EnsembleName == m.EnsembleName {
			reachable = m.ReachablePeers
		}
	}

	hits, err := s.store.search(r.Context(), searchArgs{
		Namespace:         id.Namespace,
		Scope:             req.Scope,
		AgentName:         req.AgentName,
		EnsembleName:      req.EnsembleName,
		Query:             req.Query,
		QueryEmbed:        qv,
		TopK:              req.TopK,
		Tags:              req.Tags,
		MaxAge:            time.Duration(req.MaxAgeHours) * time.Hour,
		AllowVisibilities: allowVis,
		ReachablePeers:    reachable,
	})
	if err != nil {
		slog.Error("search", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": hits})
}

func (s *server) handleList(w http.ResponseWriter, r *http.Request) {
	id, ok := identityFromCtx(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no identity")
		return
	}
	q := r.URL.Query()
	scope := q.Get("scope")
	agentName := q.Get("agentName")
	ensembleName := q.Get("ensembleName")
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))

	// Admin SAs may target any namespace; non-admins are pinned to their own.
	namespace := id.Namespace
	if id.IsAdmin {
		if ns := q.Get("namespace"); ns != "" {
			namespace = ns
		}
	}

	m, err := s.membership.Resolve(r.Context(), id)
	if err != nil && !id.IsAdmin {
		writeErr(w, http.StatusForbidden, "membership: "+err.Error())
		return
	}

	var allowVis []string
	var reachable []reachablePeer
	switch {
	case id.IsAdmin:
		allowVis = []string{"public", "trusted", "private"}
	case scope == "agent":
		if agentName == "" {
			agentName = m.AgentName
		}
		allowVis = visibilitiesFor(m.AgentName, agentName, m.TrustPeers)
	case scope == "ensemble":
		if ensembleName == "" {
			ensembleName = m.EnsembleName
		}
		if ensembleName != m.EnsembleName {
			writeErr(w, http.StatusForbidden, "not a member of "+ensembleName)
			return
		}
		allowVis = []string{"public", "trusted"}
		reachable = m.ReachablePeers
	default:
		writeErr(w, http.StatusBadRequest, "scope must be 'agent' or 'ensemble'")
		return
	}

	rows, err := s.store.list(r.Context(), listArgs{
		Namespace:         namespace,
		Scope:             scope,
		AgentName:         agentName,
		EnsembleName:      ensembleName,
		AllowVisibilities: allowVis,
		Limit:             limit,
		Offset:            offset,
		ReachablePeers:    reachable,
	})
	if err != nil {
		slog.Error("list", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": rows})
}

func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	id, ok := identityFromCtx(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no identity")
		return
	}
	q := r.URL.Query()
	scope := q.Get("scope")
	agentName := q.Get("agentName")
	ensembleName := q.Get("ensembleName")

	m, err := s.membership.Resolve(r.Context(), id)
	if err != nil && !id.IsAdmin {
		writeErr(w, http.StatusForbidden, "membership: "+err.Error())
		return
	}
	switch scope {
	case "agent":
		if agentName == "" {
			agentName = m.AgentName
		}
	case "ensemble":
		if ensembleName == "" {
			ensembleName = m.EnsembleName
		}
		if !id.IsAdmin && ensembleName != m.EnsembleName {
			writeErr(w, http.StatusForbidden, "not a member of "+ensembleName)
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "scope must be 'agent' or 'ensemble'")
		return
	}

	st, err := s.store.stats(r.Context(), id.Namespace, scope, agentName, ensembleName)
	if err != nil {
		slog.Error("stats", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	resp := map[string]any{
		"scope":          st.Scope,
		"agentName":      st.AgentName,
		"ensembleName":   st.EnsembleName,
		"count":          st.Count,
		"embeddingModel": st.EmbeddingModel,
		"embeddingDim":   st.EmbeddingDim,
		"membership": map[string]any{
			"agentName":      m.AgentName,
			"ensembleName":   m.EnsembleName,
			"trustPeers":     m.TrustPeers,
			"reachablePeers": m.ReachablePeers,
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) handleProvenance(w http.ResponseWriter, r *http.Request) {
	id, ok := identityFromCtx(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no identity")
		return
	}
	memID := r.URL.Query().Get("id")
	if memID == "" {
		writeErr(w, http.StatusBadRequest, "id required")
		return
	}
	maxDepth, _ := strconv.Atoi(r.URL.Query().Get("maxDepth"))

	rows, err := s.store.provenance(r.Context(), id.Namespace, memID, maxDepth)
	if err != nil {
		slog.Error("provenance", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Visibility filtering: drop rows the caller cannot read.
	m, _ := s.membership.Resolve(r.Context(), id)
	filtered := rows[:0]
	for _, row := range rows {
		if id.IsAdmin {
			filtered = append(filtered, row)
			continue
		}
		var allow []string
		switch row.Scope {
		case "agent":
			allow = visibilitiesFor(m.AgentName, row.AgentName, m.TrustPeers)
		case "ensemble":
			if row.EnsembleName != m.EnsembleName {
				continue
			}
			allow = []string{"public", "trusted"}
		default:
			continue
		}
		if containsString(allow, row.Visibility) {
			filtered = append(filtered, row)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"chain": filtered})
}

func (s *server) handleAdminDeleteScope(w http.ResponseWriter, r *http.Request) {
	id, ok := identityFromCtx(r.Context())
	if !ok || !id.IsAdmin {
		writeErr(w, http.StatusForbidden, "admin only")
		return
	}
	q := r.URL.Query()
	scope := q.Get("scope")
	agentName := q.Get("agentName")
	ensembleName := q.Get("ensembleName")
	namespace := q.Get("namespace")
	if namespace == "" {
		namespace = id.Namespace
	}
	n, err := s.store.deleteScope(r.Context(), namespace, scope, agentName, ensembleName)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
}

// deleteByTagsRequest is the JSON body accepted by handleAdminDeleteByTags.
// We require an explicit non-empty RequireTags so a typo cannot turn this
// endpoint into a full-scope wipe.
type deleteByTagsRequest struct {
	Namespace    string   `json:"namespace,omitempty"`
	Scope        string   `json:"scope"`
	AgentName    string   `json:"agentName,omitempty"`
	EnsembleName string   `json:"ensembleName,omitempty"`
	RequireTags  []string `json:"requireTags"`
}

// handleAdminDeleteByTags removes every row in the chosen scope whose tag
// set contains all RequireTags. Used by the controller to garbage-collect
// orphaned ensemble seeds and similar admin-driven cleanups.
func (s *server) handleAdminDeleteByTags(w http.ResponseWriter, r *http.Request) {
	id, ok := identityFromCtx(r.Context())
	if !ok || !id.IsAdmin {
		writeErr(w, http.StatusForbidden, "admin only")
		return
	}
	var req deleteByTagsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	namespace := req.Namespace
	if namespace == "" {
		namespace = id.Namespace
	}
	n, err := s.store.deleteByTags(r.Context(), deleteByTagsArgs{
		Namespace:    namespace,
		Scope:        req.Scope,
		AgentName:    req.AgentName,
		EnsembleName: req.EnsembleName,
		RequireTags:  req.RequireTags,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
}

// ------------------ Helpers ------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}


