package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// store wraps the pgxpool with the queries the HTTP handlers need.
type store struct {
	pool          *pgxpool.Pool
	schemaDim     int
	defaultTTLDur time.Duration
}

func newStore(pool *pgxpool.Pool, schemaDim, defaultTTLDays int) *store {
	return &store{
		pool:          pool,
		schemaDim:     schemaDim,
		defaultTTLDur: time.Duration(defaultTTLDays) * 24 * time.Hour,
	}
}

// memoryRow is the shape returned to API clients.
type memoryRow struct {
	ID             string    `json:"id"`
	Namespace      string    `json:"namespace"`
	Scope          string    `json:"scope"`
	AgentName      string    `json:"agentName"`
	EnsembleName   string    `json:"ensembleName,omitempty"`
	Content        string    `json:"content"`
	Tags           []string  `json:"tags,omitempty"`
	Visibility     string    `json:"visibility"`
	SourceAgent    string    `json:"sourceAgent"`
	ParentID       string    `json:"parentId,omitempty"`
	Seq            int64     `json:"seq"`
	EmbeddingModel string    `json:"embeddingModel"`
	EmbeddingDim   int       `json:"embeddingDim"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
	ExpiresAt      *time.Time `json:"expiresAt,omitempty"`
	Score          float64   `json:"score,omitempty"`
}

// storeArgs describes a write request.
type storeArgs struct {
	Namespace      string
	Scope          string // "agent" | "ensemble"
	AgentName      string // owning agent (for scope=agent) — must equal SourceAgent
	EnsembleName   string // required when scope=ensemble
	SourceAgent    string // identity from TokenReview
	Content        string
	Embedding      []float32
	EmbeddingModel string
	Tags           []string
	Visibility     string // public|trusted|private (default: private for agent scope, public for ensemble)
	ParentID       string
	TTLDays        int // 0 = use server default
}

var (
	errDimMismatch  = errors.New("embedding dimension mismatch")
	errPoolModel    = errors.New("ensemble pool embedding model conflict")
	errScopeInvalid = errors.New("scope must be 'agent' or 'ensemble'")
)

func (s *store) insert(ctx context.Context, a storeArgs) (*memoryRow, error) {
	if a.Scope != "agent" && a.Scope != "ensemble" {
		return nil, errScopeInvalid
	}
	if len(a.Embedding) != s.schemaDim {
		return nil, fmt.Errorf("%w: got %d, schema requires %d", errDimMismatch, len(a.Embedding), s.schemaDim)
	}
	if a.Scope == "ensemble" {
		if a.EnsembleName == "" {
			return nil, errors.New("ensembleName required when scope=ensemble")
		}
		// Reject if the pool already has a row with a different model.
		var existing string
		err := s.pool.QueryRow(ctx,
			`SELECT embedding_model FROM memories
			 WHERE namespace=$1 AND scope='ensemble' AND ensemble_name=$2
			 LIMIT 1`,
			a.Namespace, a.EnsembleName,
		).Scan(&existing)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// First write — this row sets the pool's pinned model.
		case err != nil:
			return nil, fmt.Errorf("check pool model: %w", err)
		default:
			if existing != a.EmbeddingModel {
				return nil, fmt.Errorf("%w: pool=%s, request=%s", errPoolModel, existing, a.EmbeddingModel)
			}
		}
	}

	visibility := a.Visibility
	if visibility == "" {
		// Agent scope is a private silo by default — entries the agent
		// itself can always read, but trust peers and strangers cannot
		// (see visibilitiesFor). Ensemble scope defaults to public so the
		// pool is useful for sharing; private is rejected at the handler.
		if a.Scope == "agent" {
			visibility = "private"
		} else {
			visibility = "public"
		}
	}

	var ttlDur time.Duration
	switch {
	case a.TTLDays > 0:
		ttlDur = time.Duration(a.TTLDays) * 24 * time.Hour
	case s.defaultTTLDur > 0:
		ttlDur = s.defaultTTLDur
	}
	var expiresAt *time.Time
	if ttlDur > 0 {
		t := time.Now().Add(ttlDur)
		expiresAt = &t
	}

	var ensembleArg any
	if a.EnsembleName != "" {
		ensembleArg = a.EnsembleName
	}
	var parentArg any
	if a.ParentID != "" {
		parentArg = a.ParentID
	}
	if a.Tags == nil {
		a.Tags = []string{}
	}

	var row memoryRow
	err := s.pool.QueryRow(ctx, `
		INSERT INTO memories (
			namespace, scope, agent_name, ensemble_name,
			content, embedding, embedding_model, embedding_dim,
			tags, visibility, source_agent, parent_id, expires_at
		) VALUES (
			$1, $2, $3, $4,
			$5, $6::vector, $7, $8,
			$9, $10, $11, $12, $13
		)
		RETURNING id, namespace, scope, agent_name, COALESCE(ensemble_name, ''),
		          content, tags, visibility, source_agent,
		          COALESCE(parent_id::text, ''), seq,
		          embedding_model, embedding_dim,
		          created_at, updated_at, expires_at`,
		a.Namespace, a.Scope, a.AgentName, ensembleArg,
		a.Content, vectorLiteral(a.Embedding), a.EmbeddingModel, s.schemaDim,
		a.Tags, visibility, a.SourceAgent, parentArg, expiresAt,
	).Scan(
		&row.ID, &row.Namespace, &row.Scope, &row.AgentName, &row.EnsembleName,
		&row.Content, &row.Tags, &row.Visibility, &row.SourceAgent,
		&row.ParentID, &row.Seq,
		&row.EmbeddingModel, &row.EmbeddingDim,
		&row.CreatedAt, &row.UpdatedAt, &row.ExpiresAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert memory: %w", err)
	}
	return &row, nil
}

// searchArgs describes a hybrid search request.
type searchArgs struct {
	Namespace    string
	Scope        string
	AgentName    string
	EnsembleName string
	Query        string
	QueryEmbed   []float32
	TopK         int
	Tags         []string // optional include filter (any-of)
	MaxAge       time.Duration
	// Visibility filter is computed by the caller (handler) based on
	// whether the requester is the owner / a trusted peer / a stranger.
	AllowVisibilities []string
	// ReachablePeers, if non-empty, extends an ensemble-scope search to
	// also include rows from peer ensembles (potentially in other
	// namespaces) that the caller is permitted to read via membrane
	// Export/Import. Each peer carries one or more accessClauses derived
	// from the matching (Import, Export) rule pairs; a candidate peer row
	// must satisfy at least one clause. Per-clause filters: Visibilities
	// (default ["public"], "private" stripped), Export.Tags any-of, and
	// Import.Tags any-of. Ignored for non-ensemble scope.
	ReachablePeers []reachablePeer
}

// buildEnsemblePeerPredicate emits the WHERE fragment for the ensemble
// scope when reachable peers are present. The fragment is a single
// parenthesised OR of:
//
//   - the "own ensemble" branch (rows from the caller's own ensemble,
//     subject to AllowVisibilities at $3 and namespace at $1), and
//   - one branch per peer, in turn an OR of per-(import,export) clauses
//     that constrain visibility and tag overlap.
//
// nextArg is the next free positional placeholder; the returned int is
// nextArg advanced past every appended arg. The returned []any contains
// only the args appended for the peer branches (peer names/namespaces +
// per-clause visibility and tag slices). The own-ensemble arg ($ownArg)
// is assumed to have already been appended by the caller.
func buildEnsemblePeerPredicate(
	ownArg int,
	peers []reachablePeer,
	nextArg int,
) (string, []any, int) {
	var addArgs []any
	parts := make([]string, 0, 1+len(peers))
	parts = append(parts, fmt.Sprintf(
		"(namespace = $1 AND ensemble_name = $%d AND visibility = ANY($3))",
		ownArg,
	))
	for _, p := range peers {
		nsArg := nextArg
		nameArg := nextArg + 1
		addArgs = append(addArgs, p.Ensemble.Namespace, p.Ensemble.Name)
		nextArg += 2

		clauseParts := make([]string, 0, len(p.Clauses))
		for _, cl := range p.Clauses {
			condParts := []string{fmt.Sprintf("visibility = ANY($%d)", nextArg)}
			addArgs = append(addArgs, cl.AllowVisibilities)
			nextArg++
			if len(cl.ImportTagsAny) > 0 {
				condParts = append(condParts, fmt.Sprintf("tags && $%d", nextArg))
				addArgs = append(addArgs, cl.ImportTagsAny)
				nextArg++
			}
			if len(cl.ExportTagsAny) > 0 {
				condParts = append(condParts, fmt.Sprintf("tags && $%d", nextArg))
				addArgs = append(addArgs, cl.ExportTagsAny)
				nextArg++
			}
			clauseParts = append(clauseParts, "("+strings.Join(condParts, " AND ")+")")
		}
		if len(clauseParts) == 0 {
			// Defensive: a peer with no clauses should never appear, but if
			// it does, treat it as "no rows from this peer".
			clauseParts = append(clauseParts, "FALSE")
		}
		parts = append(parts, fmt.Sprintf(
			"(namespace = $%d AND ensemble_name = $%d AND (%s))",
			nsArg, nameArg, strings.Join(clauseParts, " OR "),
		))
	}
	return "(" + strings.Join(parts, " OR ") + ")", addArgs, nextArg
}

// search runs hybrid retrieval: pgvector cosine similarity + tsvector
// keyword match, fused with Reciprocal Rank Fusion (k=60).
func (s *store) search(ctx context.Context, a searchArgs) ([]memoryRow, error) {
	if a.TopK <= 0 {
		a.TopK = 5
	}
	if len(a.QueryEmbed) != s.schemaDim {
		return nil, fmt.Errorf("%w: query has %d, schema requires %d", errDimMismatch, len(a.QueryEmbed), s.schemaDim)
	}
	if len(a.AllowVisibilities) == 0 {
		a.AllowVisibilities = []string{"public"}
	}

	conds := []string{
		"scope = $2",
		"(expires_at IS NULL OR expires_at > NOW())",
	}
	args := []any{a.Namespace, a.Scope, a.AllowVisibilities}
	nextArg := 4

	switch a.Scope {
	case "agent":
		conds = append(conds, "namespace = $1", "visibility = ANY($3)",
			fmt.Sprintf("agent_name = $%d", nextArg))
		args = append(args, a.AgentName)
		nextArg++
	case "ensemble":
		ownArg := nextArg
		args = append(args, a.EnsembleName)
		nextArg++
		if len(a.ReachablePeers) == 0 {
			conds = append(conds,
				"namespace = $1",
				"visibility = ANY($3)",
				fmt.Sprintf("ensemble_name = $%d", ownArg),
			)
		} else {
			frag, addArgs, n := buildEnsemblePeerPredicate(ownArg, a.ReachablePeers, nextArg)
			conds = append(conds, frag)
			args = append(args, addArgs...)
			nextArg = n
		}
	default:
		return nil, errScopeInvalid
	}

	if len(a.Tags) > 0 {
		conds = append(conds, fmt.Sprintf("tags && $%d", nextArg))
		args = append(args, a.Tags)
		nextArg++
	}
	if a.MaxAge > 0 {
		conds = append(conds, fmt.Sprintf("created_at > NOW() - $%d::interval", nextArg))
		args = append(args, a.MaxAge.String())
		nextArg++
	}

	embArg := nextArg
	args = append(args, vectorLiteral(a.QueryEmbed))
	nextArg++

	qArg := nextArg
	args = append(args, a.Query)
	nextArg++

	limitArg := nextArg
	// Pull 4× the requested top_k from each side, then fuse.
	args = append(args, a.TopK*4)

	where := strings.Join(conds, " AND ")
	// RRF with k=60. Each side returns its top-N; rows present in only one
	// side still get a score from that side. We then sum and order desc.
	sql := fmt.Sprintf(`
		WITH base AS (
			SELECT id, namespace, scope, agent_name, COALESCE(ensemble_name, '') AS ensemble_name,
			       content, tags, visibility, source_agent,
			       COALESCE(parent_id::text, '') AS parent_id, seq,
			       embedding_model, embedding_dim,
			       created_at, updated_at, expires_at,
			       embedding, content_tsv
			FROM memories
			WHERE %s
		),
		vec AS (
			SELECT id, ROW_NUMBER() OVER (ORDER BY embedding <=> $%d::vector) AS r
			FROM base
			ORDER BY embedding <=> $%d::vector
			LIMIT $%d
		),
		kw AS (
			SELECT id, ROW_NUMBER() OVER (ORDER BY ts_rank(content_tsv, plainto_tsquery('simple', $%d)) DESC) AS r
			FROM base
			WHERE content_tsv @@ plainto_tsquery('simple', $%d)
			ORDER BY ts_rank(content_tsv, plainto_tsquery('simple', $%d)) DESC
			LIMIT $%d
		),
		fused AS (
			SELECT id, SUM(score) AS score FROM (
				SELECT id, 1.0 / (60 + r) AS score FROM vec
				UNION ALL
				SELECT id, 1.0 / (60 + r) AS score FROM kw
			) u GROUP BY id
		)
		SELECT b.id, b.namespace, b.scope, b.agent_name, b.ensemble_name,
		       b.content, b.tags, b.visibility, b.source_agent,
		       b.parent_id, b.seq,
		       b.embedding_model, b.embedding_dim,
		       b.created_at, b.updated_at, b.expires_at,
		       f.score
		FROM fused f
		JOIN base b ON b.id = f.id
		ORDER BY f.score DESC
		LIMIT %d`,
		where, embArg, embArg, limitArg, qArg, qArg, qArg, limitArg, a.TopK)

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("hybrid search: %w", err)
	}
	defer rows.Close()

	out := make([]memoryRow, 0, a.TopK)
	for rows.Next() {
		var r memoryRow
		if err := rows.Scan(
			&r.ID, &r.Namespace, &r.Scope, &r.AgentName, &r.EnsembleName,
			&r.Content, &r.Tags, &r.Visibility, &r.SourceAgent,
			&r.ParentID, &r.Seq,
			&r.EmbeddingModel, &r.EmbeddingDim,
			&r.CreatedAt, &r.UpdatedAt, &r.ExpiresAt,
			&r.Score,
		); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// listArgs filters a paginated list.
type listArgs struct {
	Namespace         string
	Scope             string
	AgentName         string
	EnsembleName      string
	AllowVisibilities []string
	Limit             int
	Offset            int
	// ReachablePeers extends ensemble-scope listings to include rows from
	// peer ensembles, gated by per-rule accessClauses. See
	// searchArgs.ReachablePeers for the full semantics.
	ReachablePeers []reachablePeer
}

func (s *store) list(ctx context.Context, a listArgs) ([]memoryRow, error) {
	if a.Limit <= 0 || a.Limit > 1000 {
		a.Limit = 100
	}
	if len(a.AllowVisibilities) == 0 {
		a.AllowVisibilities = []string{"public"}
	}
	conds := []string{
		"scope = $2",
		"(expires_at IS NULL OR expires_at > NOW())",
	}
	args := []any{a.Namespace, a.Scope, a.AllowVisibilities}
	nextArg := 4

	switch a.Scope {
	case "agent":
		conds = append(conds, "namespace = $1", "visibility = ANY($3)")
		conds = append(conds, fmt.Sprintf("agent_name = $%d", nextArg))
		args = append(args, a.AgentName)
		nextArg++
	case "ensemble":
		ownArg := nextArg
		args = append(args, a.EnsembleName)
		nextArg++
		if len(a.ReachablePeers) == 0 {
			conds = append(conds,
				"namespace = $1",
				"visibility = ANY($3)",
				fmt.Sprintf("ensemble_name = $%d", ownArg),
			)
		} else {
			frag, addArgs, n := buildEnsemblePeerPredicate(ownArg, a.ReachablePeers, nextArg)
			conds = append(conds, frag)
			args = append(args, addArgs...)
			nextArg = n
		}
	default:
		return nil, errScopeInvalid
	}

	args = append(args, a.Limit, a.Offset)
	sql := fmt.Sprintf(`
		SELECT id, namespace, scope, agent_name, COALESCE(ensemble_name, ''),
		       content, tags, visibility, source_agent,
		       COALESCE(parent_id::text, ''), seq,
		       embedding_model, embedding_dim,
		       created_at, updated_at, expires_at
		FROM memories
		WHERE %s
		ORDER BY created_at DESC
		LIMIT $%d OFFSET $%d`,
		strings.Join(conds, " AND "), nextArg, nextArg+1)

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	defer rows.Close()

	out := make([]memoryRow, 0, a.Limit)
	for rows.Next() {
		var r memoryRow
		if err := rows.Scan(
			&r.ID, &r.Namespace, &r.Scope, &r.AgentName, &r.EnsembleName,
			&r.Content, &r.Tags, &r.Visibility, &r.SourceAgent,
			&r.ParentID, &r.Seq,
			&r.EmbeddingModel, &r.EmbeddingDim,
			&r.CreatedAt, &r.UpdatedAt, &r.ExpiresAt,
		); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// scopeStats reports row counts and pinned model.
type scopeStats struct {
	Scope          string `json:"scope"`
	AgentName      string `json:"agentName,omitempty"`
	EnsembleName   string `json:"ensembleName,omitempty"`
	Count          int64  `json:"count"`
	EmbeddingModel string `json:"embeddingModel,omitempty"`
	EmbeddingDim   int    `json:"embeddingDim,omitempty"`
}

func (s *store) stats(ctx context.Context, namespace, scope, agentName, ensembleName string) (*scopeStats, error) {
	var st scopeStats
	st.Scope = scope
	switch scope {
	case "agent":
		st.AgentName = agentName
		err := s.pool.QueryRow(ctx, `
			SELECT COUNT(*), COALESCE(MAX(embedding_model), ''), COALESCE(MAX(embedding_dim), 0)
			FROM memories
			WHERE namespace=$1 AND scope='agent' AND agent_name=$2`,
			namespace, agentName,
		).Scan(&st.Count, &st.EmbeddingModel, &st.EmbeddingDim)
		if err != nil {
			return nil, err
		}
	case "ensemble":
		st.EnsembleName = ensembleName
		err := s.pool.QueryRow(ctx, `
			SELECT COUNT(*), COALESCE(MAX(embedding_model), ''), COALESCE(MAX(embedding_dim), 0)
			FROM memories
			WHERE namespace=$1 AND scope='ensemble' AND ensemble_name=$2`,
			namespace, ensembleName,
		).Scan(&st.Count, &st.EmbeddingModel, &st.EmbeddingDim)
		if err != nil {
			return nil, err
		}
	default:
		return nil, errScopeInvalid
	}
	return &st, nil
}

// provenance walks the parent chain up to maxDepth steps.
func (s *store) provenance(ctx context.Context, namespace, id string, maxDepth int) ([]memoryRow, error) {
	if maxDepth <= 0 || maxDepth > 20 {
		maxDepth = 10
	}
	sql := `
		WITH RECURSIVE chain AS (
			SELECT m.*, 0 AS depth
			FROM memories m
			WHERE m.id = $1 AND m.namespace = $2
			UNION ALL
			SELECT m.*, c.depth + 1
			FROM memories m
			JOIN chain c ON m.id = c.parent_id
			WHERE c.depth < $3
		)
		SELECT id, namespace, scope, agent_name, COALESCE(ensemble_name, ''),
		       content, tags, visibility, source_agent,
		       COALESCE(parent_id::text, ''), seq,
		       embedding_model, embedding_dim,
		       created_at, updated_at, expires_at
		FROM chain
		ORDER BY depth ASC`
	rows, err := s.pool.Query(ctx, sql, id, namespace, maxDepth)
	if err != nil {
		return nil, fmt.Errorf("provenance: %w", err)
	}
	defer rows.Close()

	out := make([]memoryRow, 0)
	for rows.Next() {
		var r memoryRow
		if err := rows.Scan(
			&r.ID, &r.Namespace, &r.Scope, &r.AgentName, &r.EnsembleName,
			&r.Content, &r.Tags, &r.Visibility, &r.SourceAgent,
			&r.ParentID, &r.Seq,
			&r.EmbeddingModel, &r.EmbeddingDim,
			&r.CreatedAt, &r.UpdatedAt, &r.ExpiresAt,
		); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// deleteScope wipes every row in a scope. Admin only.
func (s *store) deleteScope(ctx context.Context, namespace, scope, agentName, ensembleName string) (int64, error) {
	switch scope {
	case "agent":
		cmd, err := s.pool.Exec(ctx,
			`DELETE FROM memories WHERE namespace=$1 AND scope='agent' AND agent_name=$2`,
			namespace, agentName,
		)
		if err != nil {
			return 0, err
		}
		return cmd.RowsAffected(), nil
	case "ensemble":
		cmd, err := s.pool.Exec(ctx,
			`DELETE FROM memories WHERE namespace=$1 AND scope='ensemble' AND ensemble_name=$2`,
			namespace, ensembleName,
		)
		if err != nil {
			return 0, err
		}
		return cmd.RowsAffected(), nil
	default:
		return 0, errScopeInvalid
	}
}

// deleteByTagsArgs describes a tag-filtered delete request.
type deleteByTagsArgs struct {
	Namespace    string
	Scope        string // "agent" | "ensemble"
	AgentName    string // required when scope=agent
	EnsembleName string // required when scope=ensemble
	RequireTags  []string // row must contain ALL of these tags (tags @> $)
}

// deleteByTags deletes every row in the given scope whose tag set is a
// superset of RequireTags. RequireTags must be non-empty; we refuse to
// degrade silently into a full-scope wipe (callers should use deleteScope
// for that).
func (s *store) deleteByTags(ctx context.Context, a deleteByTagsArgs) (int64, error) {
	if len(a.RequireTags) == 0 {
		return 0, errors.New("deleteByTags: requireTags must be non-empty")
	}
	switch a.Scope {
	case "agent":
		if a.AgentName == "" {
			return 0, errors.New("deleteByTags: agentName required for scope=agent")
		}
		cmd, err := s.pool.Exec(ctx,
			`DELETE FROM memories
			 WHERE namespace=$1 AND scope='agent' AND agent_name=$2 AND tags @> $3`,
			a.Namespace, a.AgentName, a.RequireTags,
		)
		if err != nil {
			return 0, err
		}
		return cmd.RowsAffected(), nil
	case "ensemble":
		if a.EnsembleName == "" {
			return 0, errors.New("deleteByTags: ensembleName required for scope=ensemble")
		}
		cmd, err := s.pool.Exec(ctx,
			`DELETE FROM memories
			 WHERE namespace=$1 AND scope='ensemble' AND ensemble_name=$2 AND tags @> $3`,
			a.Namespace, a.EnsembleName, a.RequireTags,
		)
		if err != nil {
			return 0, err
		}
		return cmd.RowsAffected(), nil
	default:
		return 0, errScopeInvalid
	}
}

// pruneExpired deletes rows past their TTL. Returns the number deleted.
func (s *store) pruneExpired(ctx context.Context) (int64, error) {
	cmd, err := s.pool.Exec(ctx,
		`DELETE FROM memories WHERE expires_at IS NOT NULL AND expires_at < NOW()`,
	)
	if err != nil {
		return 0, err
	}
	return cmd.RowsAffected(), nil
}

// vectorLiteral encodes a []float32 in the textual representation
// pgvector accepts: `[v1,v2,...]`.
func vectorLiteral(v []float32) string {
	var b strings.Builder
	b.Grow(len(v) * 9)
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'f', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
