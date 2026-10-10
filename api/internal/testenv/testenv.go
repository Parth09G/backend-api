// Package testenv starts a throw-away PostgreSQL in Docker with the real
// Supabase migrations applied, for integration tests (NFR-08).
//
// What Supabase normally provides (auth schema, anon/authenticated roles,
// extensions schema) is stubbed. Tests connect as billing_api like the API.
package testenv

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

const supabaseStub = `
create role anon nologin;
create role authenticated nologin;
create schema auth;
create table auth.users (id uuid primary key, email text);
create schema if not exists extensions;
`

// APIPassword is the billing_api password set in the test database.
const APIPassword = "test"

type DB struct {
	SuperURL string // postgres superuser (setup, auth.users)
	APIURL   string // billing_api role, as the API connects
	stop     func()
}

func (d *DB) Close() { d.stop() }

// Start launches PostgreSQL 17 and applies every migration. With seed=true
// supabase/seed.sql is applied too.
func Start(ctx context.Context, seed bool) (*DB, error) {
	// Ryuk (the reaper container) does not start on Docker Desktop for Windows.
	if os.Getenv("TESTCONTAINERS_RYUK_DISABLED") == "" {
		os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	}
	ctr, err := postgres.Run(ctx, "postgres:17-alpine",
		postgres.WithDatabase("app"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("postgres"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		return nil, fmt.Errorf("start postgres: %w", err)
	}
	stop := func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = ctr.Terminate(c)
	}
	superURL, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		stop()
		return nil, err
	}
	if err := Apply(ctx, superURL, seed); err != nil {
		stop()
		return nil, err
	}
	u, _ := url.Parse(superURL)
	u.User = url.UserPassword("billing_api", APIPassword)
	return &DB{SuperURL: superURL, APIURL: u.String(), stop: stop}, nil
}

// Apply runs the Supabase stub, the migrations and optionally the seed.
func Apply(ctx context.Context, superURL string, seed bool) error {
	conn, err := pgx.Connect(ctx, superURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, supabaseStub); err != nil {
		return fmt.Errorf("supabase stub: %w", err)
	}
	dir := filepath.Join(RepoRoot(), "supabase", "migrations")
	files, _ := filepath.Glob(filepath.Join(dir, "*.sql"))
	sort.Strings(files)
	if len(files) == 0 {
		return fmt.Errorf("no migrations in %s", dir)
	}
	if seed {
		files = append(files, filepath.Join(RepoRoot(), "supabase", "seed.sql"))
	}
	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		// Each file in its own transaction, like the Supabase CLI.
		if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, string(sql))
			return err
		}); err != nil {
			return fmt.Errorf("apply %s: %w", filepath.Base(f), err)
		}
	}
	_, err = conn.Exec(ctx, fmt.Sprintf(`alter role billing_api with password '%s'`, APIPassword))
	return err
}

// RepoRoot is the repository root (parent of api/).
func RepoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}
