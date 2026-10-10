// Package jobs runs background work on River, a PostgreSQL-backed queue
// (no Redis). River's tables live in the "river" schema; billing_api applies
// River's own migrations on start.
//
// User-visible work (imports, exports, big reports) is also tracked in
// app.job so the client can poll GET /jobs/{id} (API design 2.6).
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"billing-api/internal/api"
	"billing-api/internal/app"
	"billing-api/internal/auth"
	"billing-api/internal/db/dbq"
	"billing-api/internal/httpx"
)

const Schema = "river"

// Migrate brings River's queue tables up to date.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	m, err := rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{Schema: Schema})
	if err != nil {
		return err
	}
	if _, err := m.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("river migrate: %w", err)
	}
	return nil
}

// NewClient builds the River client. With work=false it can only enqueue.
func NewClient(pool *pgxpool.Pool, logger *slog.Logger, workers *river.Workers, work bool) (*river.Client[pgx.Tx], error) {
	cfg := &river.Config{Schema: Schema, Logger: logger, Workers: workers}
	if work {
		cfg.Queues = map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 5}}
		cfg.PeriodicJobs = []*river.PeriodicJob{
			river.NewPeriodicJob(river.PeriodicInterval(24*time.Hour),
				func() (river.JobArgs, *river.InsertOpts) { return StockCheckArgs{}, nil }, nil),
			river.NewPeriodicJob(river.PeriodicInterval(6*time.Hour),
				func() (river.JobArgs, *river.InsertOpts) { return PurgeArgs{}, nil }, nil),
		}
	}
	return river.NewClient(riverpgxv5.New(pool), cfg)
}

// Register adds the housekeeping workers.
func Register(w *river.Workers, a *app.App) {
	river.AddWorker(w, &StockCheckWorker{a: a})
	river.AddWorker(w, &purgeWorker{a: a})
}

// ---------------------------------------------------------------- tracking

// Create records a user-visible job (status queued).
func Create(ctx context.Context, q *dbq.Queries, kind string, params any) (dbq.Job, error) {
	b, err := json.Marshal(params)
	if err != nil {
		return dbq.Job{}, err
	}
	return q.InsertJob(ctx, dbq.InsertJobParams{Kind: kind, Params: b, CreatedBy: auth.UserID(ctx)})
}

// ToAPI renders a job with a signed download link for its result file.
func ToAPI(ctx context.Context, a *app.App, j dbq.Job) api.Job {
	out := api.Job{ID: j.ID, Kind: j.Kind, Status: j.Status, Progress: j.Progress, Error: j.Error,
		CreatedAt: j.CreatedAt, FinishedAt: j.FinishedAt}
	if j.ResultPath != nil && j.Status == "succeeded" && a.Storage != nil {
		if url, _, err := a.Storage.SignedURL(ctx, *j.ResultPath, 10*time.Minute); err == nil {
			out.ResultURL = &url
		} else {
			slog.WarnContext(ctx, "job result url", "err", err, "job", j.ID)
		}
	}
	return out
}

// Accepted writes 202 with the job.
func Accepted(w http.ResponseWriter, r *http.Request, a *app.App, j dbq.Job) {
	httpx.JSON(w, http.StatusAccepted, ToAPI(r.Context(), a, j))
}

// Outcome is what a tracked job produced.
type Outcome struct {
	Result any
	Path   string // file in Storage, if any
}

// Track runs fn for a tracked job and records running / succeeded / failed.
// The returned error is only for River's retry decision.
func Track(ctx context.Context, a *app.App, jobID uuid.UUID, fn func(ctx context.Context) (Outcome, error)) error {
	if err := a.Q.StartJob(ctx, jobID); err != nil {
		return err
	}
	out, err := fn(ctx)
	if err != nil {
		msg := err.Error()
		if ferr := a.Q.FailJob(ctx, dbq.FailJobParams{ID: jobID, Error: &msg}); ferr != nil {
			slog.ErrorContext(ctx, "record job failure", "err", ferr)
		}
		return river.JobCancel(err) // user-visible jobs are not retried blindly
	}
	var res []byte
	if out.Result != nil {
		res, _ = json.Marshal(out.Result)
	}
	var path *string
	if out.Path != "" {
		path = &out.Path
	}
	return a.Q.FinishJob(ctx, dbq.FinishJobParams{ID: jobID, Result: res, ResultPath: path})
}

// Routes mounts GET /jobs/{id}.
func Routes(a *app.App) func(chi.Router) {
	return func(r chi.Router) {
		r.Get("/{id}", httpx.H(func(w http.ResponseWriter, r *http.Request) error {
			id, err := httpx.PathUUID(r, "id")
			if err != nil {
				return err
			}
			j, err := a.Q.GetJob(r.Context(), id)
			if err != nil {
				return httpx.NotFound("job")
			}
			httpx.JSON(w, http.StatusOK, ToAPI(r.Context(), a, j))
			return nil
		}))
	}
}

// ---------------------------------------------------------------- housekeeping

// PurgeArgs deletes idempotency keys older than 7 days (API design 2.4).
type PurgeArgs struct{}

func (PurgeArgs) Kind() string { return "purge_idempotency" }

type purgeWorker struct {
	river.WorkerDefaults[PurgeArgs]
	a *app.App
}

func (w *purgeWorker) Work(ctx context.Context, _ *river.Job[PurgeArgs]) error {
	n, err := w.a.Q.PurgeIdempotencyKeys(ctx)
	if err == nil {
		slog.InfoContext(ctx, "purged idempotency keys", "rows", n)
	}
	return err
}
