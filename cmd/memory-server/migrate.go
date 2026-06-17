package main

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"text/template"
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
func runMigrate(ctx context.Context, cfg *Config) error {
	pool, err := newPool(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}

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
