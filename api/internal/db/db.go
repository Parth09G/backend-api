// Package db owns the PostgreSQL connection pool and transaction helper.
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	pgxdecimal "github.com/jackc/pgx-shopspring-decimal"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect opens a pool as the billing_api role. Use Supabase's *session*
// pooler (port 5432) so prepared statements and River's LISTEN work.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "app,extensions"
	cfg.ConnConfig.RuntimeParams["timezone"] = "Asia/Kolkata"
	cfg.ConnConfig.RuntimeParams["application_name"] = "billing-api"
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "15000" // API design 2.7
	cfg.MaxConns = 10
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.AfterConnect = func(_ context.Context, c *pgx.Conn) error {
		pgxdecimal.Register(c.TypeMap()) // numeric <-> shopspring/decimal
		return nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// InTx runs fn in one transaction: committed if fn returns nil, rolled back
// otherwise. All stock, bill and ledger writes go through this (NFR-02).
// Serialization failures and deadlocks are retried up to 3 times; fn must
// therefore be safe to run again (no side effects outside the transaction).
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = pgx.BeginFunc(ctx, pool, fn)
		if !retryable(err) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 50 * time.Millisecond)
	}
	return err
}

func retryable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01")
}
