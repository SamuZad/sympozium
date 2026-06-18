package main

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"text/template"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.tmpl
var migrationsFS embed.FS

// migrationVars are the substitution values rendered into every embedded
// migration template. The set is intentionally tiny — anything more
// complex than "what dimension is the pgvector column?" should be its
// own migration file, not a template knob.
type migrationVars struct {
	Dimension int
}

// runMigrate renders every embedded migration template with values from
// cfg and applies them to the configured database. Migrations are
// idempotent (`CREATE … IF NOT EXISTS`) so running this from many places
// concurrently — or repeatedly — is safe.
//
// On startup the database is often not yet reachable (StatefulSet still
// rolling, IAM token still warming up, …). Connection failures are
// retried every connectRetryInterval until the context is cancelled or
// the Job's activeDeadlineSeconds elapses; a fixed cadence is easier to
// reason about than the kubelet's exponential backoff. Once a usable
// connection is in hand, schema errors fail fast.
const connectRetryInterval = 10 * time.Second

func runMigrate(ctx context.Context, cfg *Config) error {
	pool, err := connectWithRetry(ctx, cfg, connectRetryInterval)
	if err != nil {
		return err
	}
	defer pool.Close()

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	vars := migrationVars{Dimension: cfg.Embedding.Dimension}
	for _, name := range names {
		data, err := fs.ReadFile(migrationsFS, "migrations/"+name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		tmpl, err := template.New(name).Option("missingkey=error").Parse(string(data))
		if err != nil {
			return fmt.Errorf("parse %s: %w", name, err)
		}
		var rendered bytes.Buffer
		if err := tmpl.Execute(&rendered, vars); err != nil {
			return fmt.Errorf("render %s: %w", name, err)
		}
		slog.Info("applying migration", "file", name, "bytes", rendered.Len(), "dimension", vars.Dimension)
		if _, err := pool.Exec(ctx, rendered.String()); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
	}
	return nil
}

// connectWithRetry opens a pgxpool and pings it, retrying every interval
// while the failure looks like the database is not yet reachable. The
// caller's ctx is the outer time bound (typically the Job's
// activeDeadlineSeconds), so this loop never runs indefinitely.
func connectWithRetry(ctx context.Context, cfg *Config, interval time.Duration) (*pgxpool.Pool, error) {
	attempt := 0
	for {
		attempt++
		pool, err := newPool(ctx, cfg)
		if err == nil {
			if pingErr := pool.Ping(ctx); pingErr == nil {
				if attempt > 1 {
					slog.Info("migrate: database reachable", "attempts", attempt)
				}
				return pool, nil
			} else {
				err = fmt.Errorf("ping: %w", pingErr)
				pool.Close()
			}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("aborted while waiting for database after %d attempts: %w", attempt, errors.Join(err, ctxErr))
		}
		slog.Warn("migrate: database not ready, retrying",
			"attempt", attempt, "interval", interval, "err", err)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("aborted while waiting for database after %d attempts: %w", attempt, errors.Join(err, ctx.Err()))
		case <-time.After(interval):
		}
	}
}
