// Package main implements the Sympozium artifact-server: a small, standalone,
// filesystem-backed blob store for agent-produced files (charts, exports).
//
// Agents upload files here over authenticated HTTP and reference them by an
// unguessable id. The reference — not the bytes — travels over NATS, so large
// attachments never touch the event bus. Channel pods download by id and
// deliver the file to the end user (e.g. a Slack upload).
//
// It deliberately shares no storage or process with the memory-server; the
// only thing it borrows is the TokenReview authentication pattern.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg := loadConfig()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, cfg); err != nil {
		logger.Error("artifact-server failed", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg *Config) error {
	st, err := newStore(cfg.DataDir)
	if err != nil {
		return err
	}

	auth, err := newAuthenticator(newK8sClient, cfg.TokenCacheSize, cfg.TokenCacheTTL, cfg.AdminServiceAccounts)
	if err != nil {
		return err
	}

	srv := newServer(cfg, st, auth.authenticate)

	go st.prunerLoop(ctx, cfg.TTL)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("artifact-server listening", "addr", cfg.Listen, "dataDir", cfg.DataDir, "maxBytes", cfg.MaxBytes, "ttl", cfg.TTL.String())
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, c := context.WithTimeout(context.Background(), 15*time.Second)
		defer c()
		return httpSrv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}
