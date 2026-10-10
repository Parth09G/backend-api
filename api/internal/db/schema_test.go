package db_test

// Database-level checks of the migrations (schema.sql + API support):
// functions, constraints and privileges, run as billing_api like the API.
// Business flows through the HTTP API are covered in internal/server tests.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"billing-api/internal/testenv"
)

func TestSchema(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: needs Docker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	env, err := testenv.Start(ctx, true) // with seed.sql
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()

	su := connect(t, env.SuperURL)
	api := connect(t, env.APIURL)

	const owner = "00000000-0000-0000-0000-000000000001"
	exec(t, su, `insert into auth.users (id, email) values ($1, 'owner@example.com')`, owner)
	exec(t, su, `insert into app.app_user (id, full_name, email, role) values ($1, 'Owner', 'owner@example.com', 'super_admin')`, owner)

	t.Run("seed applied", func(t *testing.T) {
		var items, locations int
		scan(t, api, `select count(*) from item`, nil, &items)
		scan(t, api, `select count(*) from location`, nil, &locations)
		if items != 7 || locations != 3 {
			t.Fatalf("items=%d locations=%d", items, locations)
		}
	})

	t.Run("fy and numbering honour settings prefixes", func(t *testing.T) {
		var a, b, no string
		scan(t, api, `select fn_fy('2026-04-01'), fn_fy('2027-03-31')`, nil, &a, &b)
		if a != "26-27" || b != "26-27" {
			t.Fatalf("fn_fy = %s, %s", a, b)
		}
		exec(t, api, `update shop_settings set number_prefixes = '{"receipt":"RC"}' where id = 1`)
		scan(t, api, `select fn_next_doc_no('receipt', '2026-10-10')`, nil, &no)
		if no != "RC/26-27/0001" {
			t.Fatalf("receipt no = %s", no)
		}
		scan(t, api, `select fn_next_doc_no('cash_bill', '2027-04-01')`, nil, &no)
		if no != "C/27-28/0001" {
			t.Fatalf("cash bill no = %s", no)
		}
	})

	t.Run("stock out is blocked when short", func(t *testing.T) {
		err := pgx.BeginFunc(ctx, api, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `select fn_stock_out(1, 1::smallint, 10, 'sale', 'cash_bill', 1, 1, $1)`, owner)
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Message != "INSUFFICIENT_STOCK" || pgErr.Hint == "" {
			t.Fatalf("want INSUFFICIENT_STOCK with hint, got %v", err)
		}
	})

	t.Run("purchase cost and fn_remove_cost", func(t *testing.T) {
		// Seed: item 1 has 200 @ 350 in Godown 1. Buy 100 @ 500 -> avg 400.
		exec(t, api, `select fn_stock_in(1, 2::smallint, 100, 500, 'purchase', 'purchase', 1, 1, $1)`, owner)
		var avg string
		scan(t, api, `select avg_cost::text from item where id = 1`, nil, &avg)
		if avg != "400.0000" {
			t.Fatalf("avg after purchase = %s", avg)
		}
		// Return those 100 @ 500 -> back to 350.
		exec(t, api, `select fn_stock_out(1, 2::smallint, 100, 'purchase_return', 'purchase', 1, 1, $1)`, owner)
		exec(t, api, `select fn_remove_cost(1, 100, 500)`)
		scan(t, api, `select avg_cost::text from item where id = 1`, nil, &avg)
		if avg != "350.0000" {
			t.Fatalf("avg after removal = %s", avg)
		}
	})

	t.Run("last super admin cannot be deactivated", func(t *testing.T) {
		_, err := api.Exec(ctx, `update app_user set is_active = false where id = $1`, owner)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Message != "LAST_SUPER_ADMIN" {
			t.Fatalf("want LAST_SUPER_ADMIN, got %v", err)
		}
	})

	t.Run("history is append-only for billing_api", func(t *testing.T) {
		for _, sql := range []string{
			`update stock_movement set qty_in = qty_in`,
			`delete from stock_movement`,
			`update party_ledger set debit = debit`,
			`delete from bill_revision`,
			`update einvoice_log set success = success`,
		} {
			if _, err := api.Exec(ctx, sql); pgCode(err) != "42501" {
				t.Errorf("%s: want permission denied, got %v", sql, err)
			}
		}
	})

	t.Run("browser roles cannot reach business tables", func(t *testing.T) {
		for _, role := range []string{"anon", "authenticated"} {
			err := pgx.BeginFunc(ctx, su, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, "set local role "+role); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `select 1 from app.item`)
				return err
			})
			if pgCode(err) != "42501" {
				t.Errorf("%s: want permission denied, got %v", role, err)
			}
		}
	})
}

func connect(t *testing.T, url string) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

func exec(t *testing.T, c *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := c.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func scan(t *testing.T, c *pgx.Conn, sql string, args []any, dest ...any) {
	t.Helper()
	if err := c.QueryRow(context.Background(), sql, args...).Scan(dest...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
