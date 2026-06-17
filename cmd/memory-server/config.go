package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds runtime configuration loaded from environment variables.
//
// Keys map onto Helm values under `memory.*` and `postgres.external.*`:
//   DATABASE_URL              — Postgres connection string (required).
//   MEMORY_LISTEN             — listen address (default ":8080").
//   MEMORY_NAMESPACE          — namespace the server runs in (default
//                                "sympozium-system").
//   MEMORY_EMBEDDING_PROVIDER — openai | azure_openai | ollama (default
//                                "openai").
//   MEMORY_EMBEDDING_MODEL    — model id (default "text-embedding-3-small").
//   MEMORY_EMBEDDING_DIM      — declared vector dimension; must match the
//                                schema column (default 1536).
//   MEMORY_EMBEDDING_BASE_URL — OpenAI-compatible base URL (Ollama, vLLM…).
//   MEMORY_EMBEDDING_API_KEY  — embedding API key (env-injected by the chart
//                                from the operator-supplied Secret).
//   MEMORY_ADMIN_SAS          — comma-separated list of
//                                "namespace/serviceaccount" identities that
//                                are allowed to call admin endpoints and
//                                bypass scope filters.
//   MEMORY_DEFAULT_TTL_DAYS   — cluster-wide default TTL (0 = no TTL).
//   MEMORY_TOKEN_CACHE_TTL    — TokenReview cache TTL (default 60s).
//   MEMORY_TOKEN_CACHE_SIZE   — TokenReview cache size (default 4096).
type Config struct {
	DatabaseURL string

	// DBAuthMode controls how the pool authenticates each connection.
	// Empty / "password" / "none" -> use DATABASE_URL as-is.
	// "rds-iam"                   -> mint an AWS RDS IAM token per connection.
	DBAuthMode string
	AWSRegion  string // optional override for the RDS IAM hook

	Listen    string
	Namespace string

	Embedding EmbeddingProviderConfig

	// AdminServiceAccounts is a set of "<ns>/<name>" identities allowed to
	// call admin endpoints and to read across all scopes.
	AdminServiceAccounts map[string]struct{}

	DefaultTTLDays int

	// TokenCacheTTL caches TokenReview results. Keep this short because
	// it directly bounds how long a revoked SA token keeps working.
	TokenCacheTTL  time.Duration
	TokenCacheSize int

	// MembershipCacheTTL caches Agent/Ensemble lookups (who is in which
	// ensemble, who their trust peers are, what the membrane policy says).
	// Memberships change at "kubectl edit" pace, not per-request, so this
	// can be much longer than TokenCacheTTL. Default: 10 minutes.
	MembershipCacheTTL  time.Duration
	MembershipCacheSize int
}

// EmbeddingProviderConfig describes the embedding model the server uses
// for every request. Per-scope overrides come from the Agent/Ensemble CRD
// at write time via the request body; the server still validates that the
// effective dimension matches the schema column.
type EmbeddingProviderConfig struct {
	Provider  string
	Model     string
	Dimension int
	BaseURL   string
	APIKey    string
}

func loadConfig() (*Config, error) {
	cfg := &Config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		DBAuthMode:  envDefault("MEMORY_DB_AUTH", ""),
		AWSRegion:   firstNonEmpty(os.Getenv("MEMORY_AWS_REGION"), os.Getenv("AWS_REGION")),
		Listen:      envDefault("MEMORY_LISTEN", ":8080"),
		Namespace:   envDefault("MEMORY_NAMESPACE", "sympozium-system"),
		Embedding: EmbeddingProviderConfig{
			Provider:  envDefault("MEMORY_EMBEDDING_PROVIDER", "openai"),
			Model:     envDefault("MEMORY_EMBEDDING_MODEL", "text-embedding-3-small"),
			Dimension: envIntDefault("MEMORY_EMBEDDING_DIM", 1536),
			BaseURL:   os.Getenv("MEMORY_EMBEDDING_BASE_URL"),
			APIKey:    os.Getenv("MEMORY_EMBEDDING_API_KEY"),
		},
		AdminServiceAccounts: parseAdminSAs(os.Getenv("MEMORY_ADMIN_SAS")),
		DefaultTTLDays:       envIntDefault("MEMORY_DEFAULT_TTL_DAYS", 0),
		TokenCacheTTL:        envDurationDefault("MEMORY_TOKEN_CACHE_TTL", 60*time.Second),
		TokenCacheSize:       envIntDefault("MEMORY_TOKEN_CACHE_SIZE", 4096),
		MembershipCacheTTL:   envDurationDefault("MEMORY_MEMBERSHIP_CACHE_TTL", 10*time.Minute),
		MembershipCacheSize:  envIntDefault("MEMORY_MEMBERSHIP_CACHE_SIZE", 4096),
	}

	if cfg.DatabaseURL == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	if cfg.Embedding.Dimension <= 0 {
		return nil, fmt.Errorf("MEMORY_EMBEDDING_DIM must be > 0, got %d", cfg.Embedding.Dimension)
	}
	return cfg, nil
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func envIntDefault(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envDurationDefault(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func parseAdminSAs(s string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if !strings.Contains(item, "/") {
			continue
		}
		out[item] = struct{}{}
	}
	return out
}
