// Command ingress runs the durable webhook receiver.
//
// It accepts Meta webhooks, verifies them, and stores them. It does not send
// messages and does not project conversation state; that is the dispatch
// service's job. Keeping this process narrow is what lets it stay the hot path.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aucxley/chipmank/services/ingress/internal/config"
	"github.com/aucxley/chipmank/services/ingress/internal/httpapi"
	"github.com/aucxley/chipmank/services/ingress/internal/store/memory"
	"github.com/aucxley/chipmank/services/ingress/internal/store/postgres"
)

// staticResolver serves app credentials from configuration. Swapping in a
// database or secret manager changes only this type.
type staticResolver map[string][]byte

func (s staticResolver) AppCredential(_ context.Context, appID string) (httpapi.Credential, error) {
	secret, ok := s[appID]
	if !ok {
		return httpapi.Credential{}, httpapi.ErrUnknownApp
	}
	return httpapi.Credential{Secret: secret, Ref: "config:" + appID}, nil
}

func main() {
	if err := run(); err != nil {
		slog.Error("ingress exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	resolver, appender, cleanup, err := buildDeps(cfg, logger)
	if err != nil {
		return err
	}
	defer cleanup()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpapi.New(httpapi.Options{Resolve: resolver, Append: appender, Logger: logger, MaxBodyBytes: cfg.MaxBodyBytes}).Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("ingress listening",
			"addr", cfg.Addr,
			"apps", cfg.AppIDs(),
			"max_body_bytes", cfg.MaxBodyBytes,
			"durable", cfg.DatabaseURL != "",
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		// Give in-flight webhook handlers time to finish their durable write.
		// Cutting this short is exactly how a missing acknowledgement turns
		// into a lost message.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// buildDeps picks the durable stack when a database is configured and the
// in-memory one otherwise.
//
// The order matters. A missing database is never silently downgraded to the
// ephemeral store, because that would acknowledge webhooks into a buffer that
// a restart discards. The fallback only happens when no database is configured
// at all, and it still has to be asked for explicitly.
func buildDeps(cfg config.Config, logger *slog.Logger) (httpapi.SecretResolver, httpapi.Appender, func(), error) {
	noop := func() {}

	if cfg.DatabaseURL != "" {
		poolCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		pool, err := pgxpool.New(poolCtx, cfg.DatabaseURL)
		if err != nil {
			return nil, nil, noop, fmt.Errorf("postgres: connect: %w", err)
		}
		if err := pool.Ping(poolCtx); err != nil {
			pool.Close()
			return nil, nil, noop, fmt.Errorf("postgres: ping: %w", err)
		}
		logger.Info("using durable postgres store")

		var overrides map[string][]byte
		if len(cfg.Apps) > 0 {
			overrides = cfg.Apps
		}
		return postgres.NewCredentialResolver(pool, overrides),
			postgres.NewStore(pool),
			func() { pool.Close() },
			nil
	}

	store, err := memory.New(cfg.AllowEphemeralStore)
	if err != nil {
		return nil, nil, noop, fmt.Errorf(
			"%w\nhint: set INGRESS_DATABASE_URL for durable storage, or INGRESS_ALLOW_EPHEMERAL_STORE=1 to acknowledge data loss on restart",
			err)
	}
	logger.Warn("no database configured; using ephemeral in-memory store, deliveries are lost on restart")

	if len(cfg.Apps) == 0 {
		return nil, nil, noop, errors.New(
			"config: no app secrets configured and no database to resolve them from")
	}
	return staticResolver(cfg.Apps), store, noop, nil
}
