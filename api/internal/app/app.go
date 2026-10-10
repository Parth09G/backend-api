// Package app is the dependency container shared by every domain package:
// database pool, typed queries, job queue, Supabase clients and settings.
package app

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"billing-api/internal/config"
	"billing-api/internal/db"
	"billing-api/internal/db/dbq"
	"billing-api/internal/platform/secret"
	"billing-api/internal/platform/supabase"
)

// IST is India Standard Time; business dates are IST dates (NFR-06).
var IST = time.FixedZone("IST", 5*3600+1800)

type App struct {
	Cfg     config.Config
	Pool    *pgxpool.Pool
	Q       *dbq.Queries
	River   *river.Client[pgx.Tx]
	Storage supabase.Storage
	Auth    supabase.AuthAdmin
	Secrets *secret.Box
	// Now is replaceable in tests (24-hour IRN window etc.).
	Now func() time.Time
}

// Tx runs fn in one database transaction with queries bound to it.
func (a *App) Tx(ctx context.Context, fn func(tx pgx.Tx, q *dbq.Queries) error) error {
	return db.InTx(ctx, a.Pool, func(tx pgx.Tx) error {
		return fn(tx, a.Q.WithTx(tx))
	})
}

// Today is the current IST calendar date at UTC midnight (how pgx reads a
// PostgreSQL date).
func (a *App) Today() time.Time {
	return DateOf(a.Now())
}

// DateOf converts an instant to its IST calendar date.
func DateOf(t time.Time) time.Time {
	y, m, d := t.In(IST).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// DateOr returns *d, or today when d is nil.
func (a *App) DateOr(d *time.Time) time.Time {
	if d == nil {
		return a.Today()
	}
	return *d
}
