// Command api runs the HTTP API and/or the River job worker (HLD 6.2).
// In development both run in one process; in production they can be split
// into two Railway services with RUN_API / RUN_WORKER.
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

	"github.com/joho/godotenv"

	"billing-api/internal/auth"
	"billing-api/internal/config"
	"billing-api/internal/server"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	_ = godotenv.Load() // local .env if present; real environment variables win
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := server.Build(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer a.Pool.Close()

	if cfg.RunWorker {
		if err := a.River.Start(ctx); err != nil {
			return err
		}
		slog.Info("worker started")
	}

	var srv *http.Server
	errCh := make(chan error, 1)
	if cfg.RunAPI {
		verifier, err := auth.NewVerifier(ctx, cfg.JWKSURL(), cfg.JWTIssuer())
		if err != nil {
			return err
		}
		srv = &http.Server{
			Addr:              ":" + cfg.Port,
			Handler:           server.Router(a, verifier),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       60 * time.Second,
			WriteTimeout:      90 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		go func() {
			slog.Info("api listening", "port", cfg.Port, "env", cfg.Env, "version", server.Version)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}

	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if srv != nil {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("http shutdown", "err", err)
		}
	}
	if cfg.RunWorker {
		if err := a.River.Stop(shutdownCtx); err != nil {
			slog.Error("worker shutdown", "err", err)
		}
	}
	return nil
}
