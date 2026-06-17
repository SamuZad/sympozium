// Package main implements the Sympozium central memory server.
//
// The binary has two modes:
//
//   memory-server migrate   — apply the SQL schema and exit (intended as a
//                              one-shot Helm install/upgrade Job; safe to
//                              run from many places concurrently because
//                              the SQL is idempotent).
//   memory-server serve     — run the HTTP API (default; intended as a
//                              Deployment that can scale to many replicas
//                              once the schema has been applied).
//
// Splitting migration from serving prevents replica races and lets the
// schema be applied before any pod starts serving.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	mode := "serve"
	if len(os.Args) > 1 && !startsWithFlag(os.Args[1]) {
		mode = os.Args[1]
		os.Args = append(os.Args[:1], os.Args[2:]...)
	}

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("invalid configuration", "err", err)
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	switch mode {
	case "migrate":
		if err := runMigrate(ctx, cfg); err != nil {
			logger.Error("migrate failed", "err", err)
			os.Exit(1)
		}
		logger.Info("migrate complete")
	case "serve":
		if err := runServe(ctx, cfg); err != nil {
			logger.Error("serve failed", "err", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q (expected 'migrate' or 'serve')\n", mode)
		flag.Usage()
		os.Exit(2)
	}
}

func startsWithFlag(s string) bool {
	return len(s) > 0 && s[0] == '-'
}
