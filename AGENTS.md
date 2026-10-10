# AGENTS.md

Guidance for AI coding agents (and humans) working in this repository.

Sources of truth:
- **Requirements:** `Billing_Inventory_Requirements_HLD_v2.1.docx` (content v2.2). Requirement IDs (FR-*, BR-*, NFR-*, O-*) refer to it; cite them in code comments and commits when relevant.
- **API conventions:** `Billing_API_Design_v1.docx`.
- **REST contract:** `openapi.yaml`. The tests validate every response against it.
- **Database design:** `schema.sql` (it is the first migration, verbatim).

## Project

Billing and inventory for a construction-material retailer: one shop, two godowns, up to 10 users and 3 at once. GST invoices with e-invoicing, customer and dealer ledgers, Tally import and export.

```
openapi.yaml            the API contract (102 operations)
schema.sql              design schema (copied verbatim into the first migration)
api/                    Go API + River worker (Railway)
  cmd/api/              entrypoint: HTTP server and/or worker (RUN_API / RUN_WORKER)
  internal/server/      dependency wiring, router, middleware; e2e + Postman tests
  internal/api/         request/response types (= components.schemas); Money/Qty/Rate/Date JSON types
  internal/httpx/       problem+json errors, handler adapter H, decode+validate, paging, params
  internal/auth/        Supabase JWT (JWKS), app_user loading, RequireRole/SuperAdmin, permission keys
  internal/idem/        Idempotency-Key (stored in the document's transaction)
  internal/audit/       audit_log writes
  internal/billing/     calculator, bills (create/draft/finalize/edit/cancel/copy), PDF, share
  internal/returns/     sales returns / credit notes
  internal/purchase/    purchases, cancel, purchase returns
  internal/ledger/      parties, ledgers, statements, payments + allocation
  internal/inventory/   locations, units, categories, items (+Excel import), transfers, adjustments, counts, shortage check
  internal/einvoice/    GSP Provider interface, mock provider, NIC payload, River jobs
  internal/report/      dashboard + the 21 reports (json/xlsx/pdf)
  internal/tally/       Tally XML export, Tally XML/Excel import
  internal/system/      search, GST states, notifications, audit log
  internal/docs/        PDF rendering and Storage publishing
  internal/jobs/        River client, job tracking (GET /jobs/{id}), housekeeping workers
  internal/platform/    supabase (Auth admin + Storage client), secret (AES-GCM)
  internal/db/          pgx pool, InTx with retry; dbq/ = sqlc output (DO NOT EDIT)
  internal/testenv/     PostgreSQL-in-Docker with the real migrations (tests)
  db/queries/*.sql      sqlc query files
supabase/
  migrations/           the ONLY way to change the schema (00 = schema.sql, 01 = API additions)
  seed.sql              dev-only sample data (local `supabase db reset`)
  config.toml           local stack + auth settings
postman/                generated collection (build-collection.mjs), environments, sample upload files
web/                    React 18 + TS + Vite + Tailwind + shadcn/ui (not created yet)
```

## Commands

Run all of these from `api/` unless noted:

| Task | Command |
|---|---|
| Build / vet / format | `go build ./...` · `go vet ./...` · `gofmt -l ./internal ./cmd` |
| Unit tests | `go test -short ./...` |
| All tests (Docker; Postman test also needs Node) | `go test ./...` |
| Regenerate DB code | `sqlc generate` (after editing `db/queries/*.sql` or migrations; delete stale files in `internal/db/dbq` when a query file is removed) |
| Run locally | `go run ./cmd/api` (reads `api/.env`) |
| Regenerate Postman collection (repo root) | `node postman/build-collection.mjs` |
| New migration (repo root) | `supabase migration new <name>` |
| Apply to linked project (repo root) | `supabase db push` |
| Local stack (repo root, Docker) | `supabase start` · `supabase db reset` |

On Windows, the Supabase CLI and sqlc are in `%LOCALAPPDATA%\Programs\devtools`. testcontainers needs `TESTCONTAINERS_RYUK_DISABLED=true` on Docker Desktop for Windows; `internal/testenv` sets it.

## Architecture rules (do not break)

1. **The browser never touches business data.** It uses Supabase only to sign in. Every read and write goes through the Go API (NFR-03).
2. **Schema `app` is private.** Never add it (or `river`) to the Data API's exposed schemas, and never grant `anon` or `authenticated` anything on it. Each new table needs RLS enabled with a policy for `billing_api`, plus grants (see the end of the API-support migration).
3. **The API connects as `billing_api`**, never as `postgres`. The service-role key is only for Supabase Auth admin and Storage calls.
4. **One transaction per business operation** (`app.Tx` / `db.InTx`). Bill, stock and ledger changes commit or roll back together (NFR-02). The function retries on deadlock, so it must be safe to run again.
5. **Stock changes only through the SQL functions:** `fn_stock_in`, `fn_stock_out`, `fn_post_bill` / `fn_unpost_bill`, and `fn_remove_cost` for average cost on purchase cancel/return. Never `UPDATE stock_balance` directly; `qty >= 0` is a CHECK constraint (BR-02).
6. **Check shortages before stock-out:** use `inventory.CheckShortages(..., lock=true)` so that all short lines (with stock elsewhere) are reported. Bill shortages also call `fn_alert_shortage` in a **separate** transaction after the rollback (FR-CB-03).
7. **Document numbers come only from `fn_next_doc_no`.** Call it inside the same transaction as the insert, so a rollback leaves no gap (FR-BL-10).
8. **History is append-only:** `stock_movement`, `party_ledger`, `audit_log`, `bill_revision`, `einvoice_log`. Correct mistakes with reversal rows (`ReverseLedger`, `sale_reversal` movements).
9. **Permissions are enforced server-side** with `auth.RequireRole` / `auth.SuperAdmin`, matching each operation's `x-roles` in `openapi.yaml` (FR-AU-04).
10. **Document-creating POSTs go through `idem.Run`** (`Idempotency-Key`). Bill edits and cancels check `If-Match` (`lockForChange`).
11. **Portal calls (GSP) run only in River jobs**, enqueued with `River.InsertTx` in the same transaction as the change. Never call a portal inside a request.
12. **Contract first:** a new or changed endpoint updates `openapi.yaml` and `internal/api` types, adds a scenario in `internal/server/e2e_test.go`, and is added to `postman/build-collection.mjs`. The e2e harness fails on undocumented status codes and schema mismatches.

## Domain rules to remember

- **Cash bill:** reduces stock at each line's location and posts to the ledger.
- **Account bill:** never changes stock (BR-01).
  - An **Account copy** (`source_bill_id` set) does not post to the ledger, isn't counted in sales/profit/GST reports, and isn't e-invoiced (BR-04).
  - An **independent** Account bill posts to the ledger.
- **Walk-in customers** (`party_id` null) must pay in full; credit sales need a named party (BR-09). Account bills take bank modes only (FR-AB-06).
- **No master selling price:** the rate is entered on every line (BR-05). Profit uses `bill_line.unit_cost` (average cost at sale, BR-06).
- **Tax** is calculated only in `billing.Calculate`:
  - The bill discount is spread into the line taxable values.
  - CGST/SGST when the place of supply equals the shop state, otherwise IGST.
  - Round half-up per line; round the grand total to the rupee.
- **Money, rates and quantities:** `numeric` in PostgreSQL, `shopspring/decimal` in Go, decimal strings in JSON via `api.Money` (2 dp), `api.Qty` (3 dp) and `api.Rate` (4 dp). Never `float64`, except for Excel cell display.
- **Units:** stock is stored in the base unit, `base_qty = qty × factor_to_base`. Factors are frozen once the item has movements (FR-IN-07).
- **Ledger sign:** debit = the party owes the shop more. Balance > 0 is a receivable; < 0 is a payable.
- **Edits** (Super Admin, reason, If-Match):
  1. Snapshot to `bill_revision`.
  2. `fn_unpost_bill`.
  3. Cancel and reverse the bill-screen payments.
  4. Replace the lines.
  5. `fn_post_bill`.
  6. Bump the version.
  - If the bill has an IRN, queue cancel + regenerate. Once the IRN is older than `bill_edit_window_hrs`, refuse with `EINVOICE_WINDOW_CLOSED` (BR-08).
- **FY** runs April to March and is labelled like `26-27`. Numbers look like `C/26-27/0001` (≤ 16 characters).
- **Timezone:** Asia/Kolkata. Business dates are IST dates (`app.Today`).

## Conventions

- Go 1.27, `log/slog` (JSON), chi, pgx v5, sqlc, River, go-playground/validator (custom tags `money`, `qty_pos`, `gstin`, `gstrate` ... in `httpx/decode.go`).
- **SQL** goes in `db/queries/*.sql` and is accessed through sqlc. Table names are unqualified: the migrations use `SET search_path = app`, which sqlc ignores. The only exception is `internal/report/defs.go`: report tables have dynamic columns. Every report key is exercised by the e2e test.
- **sqlc nullability:** it types a `LEFT JOIN LATERAL` column as non-null; `coalesce` it in SQL (see `ListBills.account_copy_id`). Generated columns (`balance_due`, `base_qty`) come out as pointers; use `num.Z`.
- **Handlers** are `httpx.H(func(w, r) error)`. Return `*httpx.Problem` for business errors; database errors are mapped centrally in `httpx.ToProblem`.
- **Audit:** write `audit.Log` in the same transaction for creates, edits (before/after), cancellations, approvals, exports and settings changes (FR-AL-01).
- **A schema change means:**
  1. Add a **new** migration (never edit a pushed one, and never edit the schema.sql copy).
  2. Run `sqlc generate`.
  3. Extend `internal/db/schema_test.go` or the e2e test.
- **Secrets** live only in env vars (Railway/Vercel). Never commit `.env`, keys or GSP credentials.

## Open items (assumptions in force)

- O-01: an Account copy does not post to the ledger.
- O-02: only the Super Admin edits saved bills (an Admin gets 409 `EDIT_NOT_ALLOWED`).
- O-03: Account bill payments are bank modes only.
- O-04: an independent Account bill causes no stock change.
- O-07: the GSP provider is undecided. `einvoice.Provider` has a `mock` implementation only.
- O-08: the production web host is undecided.

Ask before changing any of these. The review points in `api/README.md` §12 list further decisions waiting on the owner or accountant.
