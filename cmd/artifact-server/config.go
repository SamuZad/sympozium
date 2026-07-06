package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds runtime configuration loaded from environment variables.
//
// Keys map onto Helm values under `artifact.*`:
//
//	ARTIFACT_LISTEN                   — listen address (default ":8080").
//	ARTIFACT_DATA_DIR                 — filesystem root for stored blobs
//	                                     (default "/data"). Backed by a PVC.
//	ARTIFACT_MAX_BYTES                — per-artifact upload cap in bytes
//	                                     (default 25 MiB). Artifacts never
//	                                     travel over NATS, so this can be far
//	                                     larger than the event-bus max_payload.
//	ARTIFACT_TTL                      — how long an artifact lives before the
//	                                     background sweeper deletes it
//	                                     (default 24h, mirroring the old NATS
//	                                     stream MaxAge).
//	ARTIFACT_READER_SERVICE_ACCOUNTS — optional comma-separated list of
//	                                     "namespace/serviceaccount" identities
//	                                     allowed to read ANY artifact. Escape
//	                                     hatch for edge cases (shared ensemble
//	                                     channels, the controller). The normal
//	                                     owner + sibling-channel convention
//	                                     needs no entries here.
//	ARTIFACT_ADMIN_SAS               — optional comma-separated
//	                                     "namespace/serviceaccount" identities
//	                                     that bypass read authorization.
//	ARTIFACT_TOKEN_CACHE_TTL         — TokenReview cache TTL (default 60s).
//	                                     Bounds how long a revoked token keeps
//	                                     working, so keep it short.
//	ARTIFACT_TOKEN_CACHE_SIZE        — TokenReview cache size (default 4096).
//	ARTIFACT_AGENT_SA_SUFFIX         — suffix identifying an agent's SA
//	                                     (default "-agent").
//	ARTIFACT_CHANNEL_SA_SUFFIX       — suffix identifying a channel pod's SA
//	                                     (default "-channel").
type Config struct {
	Listen   string
	DataDir  string
	MaxBytes int64
	TTL      time.Duration

	// ReaderServiceAccounts is a set of "<ns>/<name>" identities allowed to
	// read any artifact regardless of ownership.
	ReaderServiceAccounts map[string]struct{}

	// AdminServiceAccounts is a set of "<ns>/<name>" identities that bypass
	// read authorization entirely.
	AdminServiceAccounts map[string]struct{}

	TokenCacheTTL  time.Duration
	TokenCacheSize int

	AgentSASuffix   string
	ChannelSASuffix string
}

func loadConfig() *Config {
	return &Config{
		Listen:                envDefault("ARTIFACT_LISTEN", ":8080"),
		DataDir:               envDefault("ARTIFACT_DATA_DIR", "/data"),
		MaxBytes:              envInt64Default("ARTIFACT_MAX_BYTES", 25*1024*1024),
		TTL:                   envDurationDefault("ARTIFACT_TTL", 24*time.Hour),
		ReaderServiceAccounts: parseSASet(os.Getenv("ARTIFACT_READER_SERVICE_ACCOUNTS")),
		AdminServiceAccounts:  parseSASet(os.Getenv("ARTIFACT_ADMIN_SAS")),
		TokenCacheTTL:         envDurationDefault("ARTIFACT_TOKEN_CACHE_TTL", 60*time.Second),
		TokenCacheSize:        envIntDefault("ARTIFACT_TOKEN_CACHE_SIZE", 4096),
		AgentSASuffix:         envDefault("ARTIFACT_AGENT_SA_SUFFIX", "-agent"),
		ChannelSASuffix:       envDefault("ARTIFACT_CHANNEL_SA_SUFFIX", "-channel"),
	}
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
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

func envInt64Default(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
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

func parseSASet(s string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" || !strings.Contains(item, "/") {
			continue
		}
		out[item] = struct{}{}
	}
	return out
}
