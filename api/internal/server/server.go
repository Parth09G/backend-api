// Package server builds the application (dependencies, job workers) and the
// HTTP router for /api/v1 (openapi.yaml).
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/go-chi/httprate"
	"github.com/riverqueue/river"

	"billing-api/internal/app"
	"billing-api/internal/auth"
	"billing-api/internal/billing"
	"billing-api/internal/config"
	"billing-api/internal/db"
	"billing-api/internal/db/dbq"
	"billing-api/internal/einvoice"
	"billing-api/internal/httpx"
	"billing-api/internal/inventory"
	"billing-api/internal/jobs"
	"billing-api/internal/ledger"
	"billing-api/internal/platform/secret"
	"billing-api/internal/platform/supabase"
	"billing-api/internal/purchase"
	"billing-api/internal/report"
	"billing-api/internal/returns"
	"billing-api/internal/settings"
	"billing-api/internal/system"
	"billing-api/internal/tally"
	"billing-api/internal/users"
)

// Version is set at build time (-ldflags "-X billing-api/internal/server.Version=...").
var Version = "dev"

// Build connects to the database and wires every dependency and job worker.
// The River client is created but not started.
func Build(ctx context.Context, cfg config.Config, logger *slog.Logger) (*app.App, error) {
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	box, err := secret.New(cfg.EncryptionKey)
	if err != nil {
		pool.Close()
		return nil, err
	}
	sb := supabase.New(cfg.SupabaseURL, cfg.ServiceRoleKey, cfg.StorageBucket, cfg.AuthRedirect)
	a := &app.App{Cfg: cfg, Pool: pool, Q: dbq.New(pool), Storage: sb, Auth: sb, Secrets: box, Now: time.Now}

	workers := river.NewWorkers()
	jobs.Register(workers, a)
	einvoice.Register(workers, a)
	inventory.RegisterWorkers(workers, a)
	report.RegisterWorkers(workers, a)
	tally.RegisterWorkers(workers, a)
	if cfg.MigrateRiver {
		if err := jobs.Migrate(ctx, pool); err != nil {
			pool.Close()
			return nil, err
		}
	}
	a.River, err = jobs.NewClient(pool, logger, workers, cfg.RunWorker)
	if err != nil {
		pool.Close()
		return nil, err
	}
	return a, nil
}

// Router builds the HTTP handler.
func Router(a *app.App, v *auth.Verifier) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(requestIDHeader)
	r.Use(middleware.RealIP)
	r.Use(auth.ClientIPMiddleware)
	r.Use(requestLogger)
	r.Use(recoverer)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   a.Cfg.AllowedOrigins,
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Authorization", "Content-Type", "Idempotency-Key", "If-Match", "X-Request-Id"},
		ExposedHeaders:   []string{"X-Request-Id", "ETag", "Location", "Idempotent-Replay"},
		AllowCredentials: false,
		MaxAge:           300,
	}))
	r.NotFound(httpx.H(func(w http.ResponseWriter, r *http.Request) error { return httpx.NotFound("endpoint") }))
	r.MethodNotAllowed(httpx.H(func(w http.ResponseWriter, r *http.Request) error {
		return &httpx.Problem{Status: http.StatusMethodNotAllowed, Code: httpx.CodeValidation, Detail: "method not allowed"}
	}))

	health := healthHandler(a)
	r.Get("/health", health) // Railway health check
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/health", health)
		r.Group(func(r chi.Router) {
			r.Use(middleware.Timeout(60 * time.Second))
			r.Use(auth.Middleware(v, a.Q))
			r.Use(perUserLimit(600, time.Minute))

			r.Get("/me", users.Me(a))
			r.With(perUserLimit(60, time.Minute)).Get("/search", system.SearchRoute(a))
			r.Get("/lookups/states", system.StatesRoute(a))
			r.Route("/jobs", jobs.Routes(a))
			r.Route("/users", users.Routes(a))
			r.Route("/settings", settings.Routes(a))
			r.Route("/locations", inventory.LocationRoutes(a))
			r.Route("/units", inventory.UnitRoutes(a))
			r.Route("/categories", inventory.CategoryRoutes(a))
			r.Route("/items", inventory.ItemRoutes(a))
			r.Route("/parties", ledger.PartyRoutes(a))
			r.Route("/bills", billing.Routes(a))
			r.Route("/payments", ledger.PaymentRoutes(a))
			r.Route("/returns", returns.Routes(a))
			r.Route("/purchases", purchase.Routes(a))
			r.Route("/purchase-returns", purchase.ReturnRoutes(a))
			r.Route("/stock-transfers", inventory.TransferRoutes(a))
			r.Route("/stock-adjustments", inventory.AdjustmentRoutes(a))
			r.Route("/stock-counts", inventory.CountRoutes(a))
			r.Route("/dashboard", report.DashboardRoutes(a))
			r.Route("/reports", report.ReportRoutes(a))
			r.Route("/notifications", system.NotificationRoutes(a))
			r.Route("/tally", tally.Routes(a))
			r.Route("/audit-log", system.AuditRoutes(a))
		})
	})
	return r
}

// healthHandler checks the database (NFR-05).
func healthHandler(a *app.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := a.Pool.Ping(ctx); err != nil {
			httpx.JSON(w, http.StatusServiceUnavailable, map[string]string{"status": "down", "db": "unreachable", "version": Version})
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok", "db": "ok", "version": Version})
	}
}

// perUserLimit rate-limits per signed-in user (API design 2.7).
func perUserLimit(n int, window time.Duration) func(http.Handler) http.Handler {
	return httprate.Limit(n, window,
		httprate.WithKeyFuncs(func(r *http.Request) (string, error) {
			if u, ok := auth.FromContext(r.Context()); ok {
				return u.ID.String(), nil
			}
			return httprate.KeyByIP(r)
		}),
		httprate.WithLimitHandler(func(w http.ResponseWriter, r *http.Request) {
			httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusTooManyRequests, Code: httpx.CodeRateLimited,
				Detail: "too many requests; slow down"})
		}))
}

func requestIDHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", middleware.GetReqID(r.Context()))
		next.ServeHTTP(w, r)
	})
}

// recoverer turns a panic into a 500 problem response.
func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				slog.ErrorContext(r.Context(), "panic", "err", fmt.Sprint(rec), "stack", string(debug.Stack()),
					"request_id", middleware.GetReqID(r.Context()))
				httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusInternalServerError, Code: httpx.CodeInternal})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		slog.InfoContext(r.Context(), "http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"ms", time.Since(start).Milliseconds(),
			"request_id", middleware.GetReqID(r.Context()),
		)
	})
}
