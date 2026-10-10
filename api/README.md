# API and database logic: review guide

This guide explains how the backend implements `openapi.yaml` on top of `schema.sql`, flow by flow, so the logic can be reviewed against:
- the requirements/HLD document (`Billing_Inventory_Requirements_HLD_v2.1.docx`, content v2.2),
- the API design document (`Billing_API_Design_v1.docx`).

Requirement IDs (FR-*, BR-*, NFR-*, O-*) refer to those documents.

**Status**
- **Built:** all 102 operations in `openapi.yaml`, plus the background workers.
- **End-to-end tests pass:** they run against PostgreSQL 17 in Docker, and every response is validated against the spec.
- **The Postman collection runs clean:** 122 requests, 0 failed assertions.
- **Still open (§12):** the GSP/ASP provider (a built-in **mock** stands in), WhatsApp/email delivery of alerts, and the nightly `pg_dump` backup.

Contents
1. [Layout](#1-layout)
2. [Request lifecycle](#2-request-lifecycle)
3. [Errors](#3-errors)
4. [Calculation rules](#4-calculation-rules)
5. [Bills](#5-bills)
6. [Payments, returns, purchases, stock](#6-payments-returns-purchases-stock)
7. [E-invoice and e-way bill](#7-e-invoice-and-e-way-bill)
8. [Catalogue, parties, users, settings](#8-catalogue-parties-users-settings)
9. [Dashboard, reports, Tally](#9-dashboard-reports-tally)
10. [Database](#10-database)
11. [Tests](#11-tests)
12. [Review points and open decisions](#12-review-points-and-open-decisions)
13. [Changes made to openapi.yaml](#13-changes-made-to-openapiyaml)

---

## 1. Layout

| Package | Responsibility |
|---|---|
| `cmd/api` | Entry point: API and/or worker (`RUN_API`, `RUN_WORKER`) |
| `internal/server` | Builds dependencies and the router; middleware; end-to-end tests |
| `internal/api` | Request/response types mirroring `components.schemas`; `Money` / `Qty` / `Rate` / `Date` JSON types |
| `internal/httpx` | Problem errors, handler adapter, JSON decode + validation, paging, query params, ETag |
| `internal/auth` | Supabase JWT (JWKS) check, user loading, roles, permission keys |
| `internal/idem` | `Idempotency-Key` handling (in the document's transaction) |
| `internal/audit` | Audit-log writes |
| `internal/billing` | Calculator, bills (create, draft, finalize, edit, cancel, copy), PDF, share, e-invoice triggers |
| `internal/returns` | Sales returns / credit notes |
| `internal/purchase` | Purchases, purchase cancel, purchase returns |
| `internal/ledger` | Parties, ledgers, statements, payments and allocation |
| `internal/inventory` | Locations, units, categories, items, Excel import, transfers, adjustments, counts, shortage check |
| `internal/einvoice` | GSP provider interface, mock provider, NIC payload, River jobs |
| `internal/report` | Dashboard and the 21 reports (JSON / Excel / PDF) |
| `internal/tally` | Tally XML export and import (XML or Excel) |
| `internal/system` | Search, GST states, notifications, audit log |
| `internal/docs` | PDF rendering (invoice, credit note, receipt, statement, report tables) and publishing |
| `internal/jobs` | River client, job tracking (`GET /jobs/{id}`), nightly stock check, idempotency-key purge |
| `internal/platform/supabase` | Auth admin (invite, delete, reset) and Storage (upload, download, signed URL) |
| `internal/platform/secret` | AES-256-GCM for stored GSP credentials |
| `internal/db` | Connection pool and transaction retry; `dbq` is sqlc output |
| `db/queries/*.sql` | All SQL except the report definitions (sqlc) |

## 2. Request lifecycle

Middleware runs in this order (API design 6.2):
1. Request ID: `X-Request-Id` is on every response and in every error body.
2. Real client IP (used for the audit log).
3. A JSON log line per request.
4. Panics are turned into a `500` problem.
5. CORS: only `CORS_ALLOWED_ORIGINS`. Exposes `ETag`, `Location` and `Idempotent-Replay`.
6. `/health` and `/api/v1/health` need no auth; they ping the database.
7. Auth (`internal/auth`):
   - The bearer token's signature is checked against the Supabase JWKS (ES256/RS256 only), along with issuer, audience and expiry.
   - `app.app_user` is loaded on every call. An unknown or inactive user gets **401** (API design 2.2).
   - A changed Supabase `session_id` seen on `GET /me` is written to the audit log as a login.
8. Rate limits per user: 600 per minute overall, 60 per minute on `/search` → `429 RATE_LIMITED`.
9. Role checks per route (`x-roles`); Super Admin routes use `auth.SuperAdmin`.
10. A 60 s handler timeout; database statements time out after 15 s.

### Transactions

`db.InTx` runs every write in one transaction and retries serialization failures and deadlocks up to 3 times (API design 6.3).

### Idempotency (`internal/idem`)

Every document-creating POST requires an `Idempotency-Key`. Inside the same transaction as the document:
1. `INSERT … ON CONFLICT DO NOTHING` reserves `(user, key)`. A duplicate request arriving concurrently blocks on the primary key until the first one commits.
2. The handler runs.
3. The response (status, body, ETag, Location) is stored with the key.

On a replay:
- Same key and same body: the stored response is returned with `Idempotent-Replay: true`.
- Same key with a different body or path: `409 IDEMPOTENCY_REPLAY_MISMATCH`.

If the first attempt failed, its transaction (and the key) rolled back, so a retry simply runs. Keys are purged after 7 days by a periodic job.

### Optimistic concurrency

- `GET /bills/{id}` returns `ETag: "<version>"`.
- `PUT /bills/{id}` and `POST /bills/{id}/cancel` require `If-Match`. A mismatch returns `412 VERSION_MISMATCH`.
- Every edit and cancel bumps the version.

## 3. Errors

Errors are RFC 9457 `application/problem+json`:

```json
{ "type": "urn:billing:error:validation-failed", "title": "...", "status": 400, "code": "VALIDATION_FAILED",
  "detail": "...", "errors": [{ "field": "lines[1].qty", "message": "..." }], "request_id": "..." }
```

| Source | Result |
|---|---|
| Struct validation (decimal-string patterns, GSTIN, GST rate set, dates) | 400 `VALIDATION_FAILED` with JSON paths |
| Business checks in Go | 409 `INVALID_STATE` / `EDIT_NOT_ALLOWED` / `EINVOICE_WINDOW_CLOSED`, 412 `VERSION_MISMATCH`, 422 `INSUFFICIENT_STOCK` / `CREDIT_LIMIT_EXCEEDED` |
| `P0001 INSUFFICIENT_STOCK` from `fn_stock_out` | 422 (safety net; Go pre-checks all lines first) |
| `23505` unique | 409 `DUPLICATE` with the field name (dealer invoice, GSTIN, SKU, unit, email, ...) |
| `23503`, `23514`, `23502`, `22P02` | 400 `VALIDATION_FAILED` (`stock_balance_qty_check` → 422) |
| `40001` / `40P01` after 3 retries | 409 `CONFLICT` |
| `57014` statement timeout | 503 |
| Anything else | 500 `INTERNAL`, logged with the request id |

## 4. Calculation rules

The calculator is in `internal/billing/calc.go`, and the server is the only place totals are computed (API design 6.4).

1. **Line gross:** `qty × rate`, rounded to paise half-up.
2. **Line discount:** `gross × discount_pct% + discount_amt`. A discount above gross → 400.
3. **GST-inclusive prices** (setting `prices_include_gst`): the net is divided by `(1 + rate%)`.
4. **Bill discount:** spread over the lines in proportion to their taxable value; the rounding remainder goes to the largest line. It reduces each line's taxable value, so GST is charged on the net. `bill.bill_discount` keeps the total for printing.
5. **GST:**
   - Intra-state: CGST = SGST = half of GST, each rounded per line.
   - Inter-state (`place_of_supply ≠ shop state`): IGST.
6. **Charges:** labour, transport and other are taxed at their own rate (default: settings `default_charge_gst`).
7. **Grand total:** rounded to the nearest rupee; `round_off` holds the difference.
8. **Amount in words:** Indian format ("One Lakh Forty Eight Thousand ...").
9. **`expected_total`:** if sent and different from the server's total → 400.

The API design's worked example (1.5 t @ 68,500 and 50 bags @ 395 less 2%, with labour and transport) gives **₹ 1,48,450.00 with round-off 0.10**, exactly as in the doc. It is one of the unit tests.

## 5. Bills

All bill writes go through one function, `billing.save`. Every path below runs in a single transaction.

### Create an active Cash bill (`POST /bills`, status `active`)

1. **`prepare` validates:**
   - Shop settings exist.
   - **Customer:** either `party_id` (active, not a dealer) or `walk_in_name` (Cash bills only). Its snapshot (name, GSTIN, address, mobile, site) is stored on the bill.
   - **Lines:**
     - Each item is active.
     - The location is the line's own, or the default billing location.
     - The unit must be set up for the item, and the quantity's decimals must suit that unit.
     - `base_qty = qty × factor` (3 decimals).
     - HSN and GST come from the item.
   - **Payments:**
     - Account bills accept bank modes only (FR-AB-06).
     - The total paid must not exceed the grand total.
     - A walk-in customer must pay the full amount (BR-09).
2. **Shortage check:**
   - Lock the `stock_balance` rows of all items in id order (no deadlocks).
   - Sum the needs per (item, location).
   - If anything is short, roll back and return 422 `INSUFFICIENT_STOCK`. It lists **every** short line, with the stock **available elsewhere**.
   - Then, **in a separate transaction**, call `fn_alert_shortage` per short line, which creates a Super Admin notification (FR-CB-02/03). The response says `alert_sent: true`.
3. **Credit limit:** party outstanding + (grand total − paid) > limit → 422 `CREDIT_LIMIT_EXCEEDED`. A Super Admin can resend with `override_credit_limit` (FR-BL-08).
4. **Number:** `fn_next_doc_no('cash_bill' | 'account_bill', bill_date)`. The series row lock makes numbering gap-free.
5. **Insert and post:**
   - Insert the bill, its lines and charges.
   - `fn_post_bill` deducts stock per line location at the average cost (stored in `bill_line.unit_cost`) and debits the customer ledger.
6. **Payments:**
   - Each payment split becomes a receipt numbered `R/…`, credited in the ledger, with `payment.bill_id` set ("taken on the bill screen").
   - It is allocated to the bill, and `amount_paid` is refreshed (bounced and cancelled payments are excluded).
7. **E-invoice:** a B2B bill (customer GSTIN) with e-invoicing on gets `einv_status = pending`, and `einvoice_generate` is queued **in the same transaction** (River `InsertTx`).
8. **Response:** an audit row is written, then `201` with `ETag: "1"` and `Location`.

### Account bills

- They are the same as Cash bills except `affects_stock = false`, so stock is never touched (BR-01). They need a named customer (FR-AB-03).
- **Independent Account bill:** posts to the ledger and counts in sales (BR-04).
- **Account copy** (`POST /bills/{id}/copy-to-account`):
  - The source must be an active Cash bill with a named customer and no active copy (a partial unique check gives 409).
  - The copy keeps `source_bill_id`, has `posts_to_ledger = false`, takes no payments, and is **not e-invoiced** (see R6).
  - Its status is `active` by default, or `draft` to review first.

### Drafts

- `status: draft` gives a bill with no number that doesn't touch stock or the ledger. Drafts can't take payments.
- Use `PUT` (with `If-Match`) to change a draft and `DELETE` to remove it.
- `POST /bills/{id}/finalize` rebuilds the request from the stored draft and runs the same save path: shortage check, number, post, e-invoice.

### Edit an active bill (`PUT /bills/{id}`)

**Checks (any failure stops the edit):**
- Super Admin only; an admin gets `409 EDIT_NOT_ALLOWED`, as the API doc specifies.
- A reason is required.
- `If-Match` must match the current version.
- The period must not be locked (`shop_settings.locked_until`; FR-ED-04).
- There must be no returns against the bill.
- If an IRN exists, it must be within `bill_edit_window_hrs` (24 h by default); otherwise `409 EINVOICE_WINDOW_CLOSED` (BR-08).

**Steps (one transaction):**
1. Snapshot the full current bill JSON into `bill_revision` (version, reason, user).
2. `fn_unpost_bill`: stock comes back at each line's original cost, and the ledger gets a reversal.
3. Payments taken on the bill screen are cancelled and reversed; the edit re-enters them.
4. Replace the lines and charges, run the shortage check, `fn_post_bill`, record the new payments, bump the version.
5. If the bill had an IRN, queue `einvoice_cancel` with `regenerate = true`. The job cancels the IRN, clears it, and queues a new generation (FR-ED-03, but see R7).

### Cancel (`POST /bills/{id}/cancel`, Super Admin, `If-Match`)

- **Refused when:**
  - an active Account copy exists (409; cancel the copy first),
  - the bill has returns (409),
  - the IRN is older than the window (409).
- **Steps:**
  1. `fn_unpost_bill`.
  2. Payments taken on the bill screen are cancelled and reversed (treated as refunded; see R1).
  3. Receipts allocated later are detached and go back on account.
  4. The status becomes cancelled; the number is kept and the version bumped.
  5. A live IRN is cancelled by a job.
- Reprints show a **CANCELLED** watermark.

### Other bill endpoints

- **`GET /bills/{id}`** returns the totals with a per-rate tax split, the payments allocated to the bill, `cost_total` and `gross_profit` (items taxable − cost), the e-invoice state with `cancel_deadline`, and `allowed_actions`. The actions depend on role, status, the IRN window, the period lock, returns and any active copy.
- **`/pdf`** renders the invoice (original/duplicate/triplicate) with the IRN QR code. While a B2B e-invoice is pending or failed it returns 409, because printing waits for the QR (FR-EI-02).
- **`/share`** builds a `wa.me` link with the PDF link.
- **`GET /bills`** filters by type, status, payment status, e-invoice status, party, location, dates and text, and can list unlinked Account bills (R-1 review). Sort options: `bill_date`, `bill_no`, `grand_total`, `created_at`.

## 6. Payments, returns, purchases, stock

### Payments (`internal/ledger`)

- **Recording:**
  - Receipts (`R/…`) credit the customer; payments (`PV/…`) debit the dealer.
  - Cheques start as `pending`.
- **Allocation:**
  - Explicit allocations are checked: right party, active, still due; Account bills need bank modes.
  - Or `auto_allocate` pays off the oldest open bills or purchases first.
  - Without either, the money stays "on account".
- **Cheque bounced:** the ledger entry is reversed and the bills reopen (FR-CU-03). A cheque that is already cleared or bounced cannot change → 409.
- **Cancel** (Super Admin): the payment is reversed in the ledger, unless it is a bounced cheque (already reversed), and its bills are refreshed.

### Returns / credit notes (`internal/returns`)

- **Create:**
  - Against an active bill, never an Account copy (409; see R3).
  - Per line, the quantity is capped at sold − earlier pending/approved returns, in base units.
  - Values are proportional to the line's taxable value, so discounts carry over; GST is split as on the bill.
  - `settlement`: `adjust_ledger` (named customers only) or `refund` (refund details stored until approval).
  - Created by an **Admin**: `pending`, plus a `return_pending` notification for Super Admins.
  - Created by the **Super Admin**: approved at once.
- **Approve** (Super Admin):
  1. Number `CN/…`.
  2. For Cash bills, restock the `restock` lines at the **original sale cost**.
  3. Credit the customer (if the bill posts to the ledger).
  4. With `refund`, record a payment voucher (it debits the customer back).
  5. If the bill was e-invoiced, queue the credit note's e-invoice (FR-RT-03).
- **Reject:** pending → rejected, with a reason.

### Purchases (`internal/purchase`)

- **Create:**
  - Checks: dealer is active and not a customer-only party; locations are valid; the duplicate dealer invoice number → 409 `DUPLICATE`.
  - Interstate comes from the dealer's state code, or can be overridden.
  - Line taxable = `qty × rate − discount`. Freight + other charges are apportioned by taxable value.
  - **Landed cost per base unit** = (taxable + share) ÷ base qty. GST is excluded, assumed claimed as input credit (R4).
  - `fn_stock_in('purchase')` updates the weighted average and `last_purchase_rate`.
  - The dealer is credited with the grand total, rounded to the rupee. An optional payment is recorded and allocated.
- **Cancel** (Super Admin):
  - Only when no purchase returns exist and the stock is still on hand at each line's location; otherwise 422 `INSUFFICIENT_STOCK`.
  - Stock goes out, and `fn_remove_cost` takes the purchase's effect back out of the average cost.
  - The ledger is reversed and payments go back on account as advances.
- **Purchase return:**
  - Quantity is capped at bought − already returned.
  - Stock goes out and `fn_remove_cost` runs.
  - The dealer is debited with the proportional value including GST.

### Stock (`internal/inventory`)

- **Transfer:** the source is locked and shortage-checked; then `transfer_out` and `transfer_in` at the same cost; the average is unchanged.
- **Adjustment:** quantities are in base units; a negative change is shortage-checked; reason `other` needs notes.
- **Physical count:** the preview compares counted with system quantity and values the difference at average cost. Posting creates a `count_correction` adjustment for the non-zero differences (400 if nothing differs).
- **Item stock ledger:** a running balance per location, with document numbers from `fn_ref_no`.

## 7. E-invoice and e-way bill

- **Provider:** behind the `einvoice.Provider` interface.
  - The credentials are stored **encrypted** (`einv_secret`, AES-256-GCM with `APP_ENCRYPTION_KEY`) and are never logged or returned.
  - Only `mock` exists today (O-07). It gives a deterministic IRN per document, an ack number, a signed-QR-like string and an e-way bill number with validity. Password `fail` simulates rejections.
- **Jobs** (River, up to 5 attempts):
  - `einvoice_generate` builds the NIC INV-01 JSON (seller, buyer, items including charges as services, totals; credit notes reference the original invoice).
  - `einvoice_cancel` optionally regenerates afterwards.
  - `ewaybill_generate`.
- **Logging:** every call is written to `einvoice_log` (request, response, success, error code) (FR-EI-05).
- **Failures:**
  - While attempts remain, the status stays `pending` with `last_error`.
  - After the last attempt it becomes `failed`, and an `einv_failed` notification goes to Super Admins.
  - A portal rejection on the last attempt is not retried again.
- **Retry:** `POST /bills/{id}/einvoice` re-queues a bill that is failed, cancelled, or was saved while e-invoicing was off.

## 8. Catalogue, parties, users, settings

- **Categories:**
  - The wizard creates a category with its units (existing, or new on the spot) and attributes in one call.
  - An attribute code can't change, and an attribute can't be deleted, while items use it.
  - A category can be deactivated only when it has no active items.
- **Items:**
  - **Attributes** are checked against the category: no unknown codes, required ones present, numbers stored as numbers, select values normalised to the option's spelling.
  - **Name:** built from the `part_of_name` attributes in order, with the value unit (e.g. "JSW 12 mm"), unless given.
  - **Duplicates:** same category and attributes → 409 `DUPLICATE` (FR-IN-04).
  - **Units:** must be allowed in the category. Once the item has movements, the base unit and existing factors can't change; new units can still be added (FR-IN-07).
  - **Opening stock:** only on create, posted via `fn_stock_in('opening')`.
- **Item import** (`POST /items/import`, async):
  - The file goes to Storage and a job validates every row (savepoint per row).
  - All-or-nothing: nothing is written if any row has errors or `dry_run` is set.
  - The job result lists the row errors.
  - The template from `GET /categories/{id}/import-template` follows the category's attributes and active locations.
- **Parties:**
  - The GSTIN's first two digits set the state code (a conflicting `state_code` → 400).
  - GSTIN is unique.
  - Balance = opening + Σ(debit − credit), where + means receivable.
  - The ledger has opening, entries and a running balance; the statement PDF shows Dr/Cr.
- **Users:**
  - **Invite:**
    1. Create the Supabase Auth user, which sends the invite email.
    2. Create the `app_user`.
    3. If step 2 fails, delete the Auth user again.
  - **Role changes and deactivation** take effect on the next call. A database trigger keeps at least one active Super Admin (409 `CONFLICT`).
- **Settings:**
  - `PUT` replaces the profile; optional fields left out keep their value.
  - Number prefixes (≤ 4 characters) apply when a document series starts a new financial year.
  - The logo is a PNG/JPG of at most 1 MB in Storage.
  - The Tally mapping is stored as JSON.

## 9. Dashboard, reports, Tally

- **Dashboard:**
  - **Sales** = grand total of active bills that post to the ledger, so copies are never counted twice.
  - **Profit** = items taxable − cost at the time of sale; margin is a % of items taxable.
  - **Receivables / payables** come from party balances.
  - **Low stock** compares stock with the reorder level (per location when one is given).
  - **Pending work:** failed or pending e-invoices, pending returns, blocked bills today, documents not yet exported to Tally, cheques due.
- **Reports:** `internal/report/defs.go` defines all 21 keys, each as SQL plus typed columns.
  - JSON is paged and returns totals.
  - `xlsx` (numbers stay numbers, Indian format) and `pdf` (landscape table) are uploaded and returned as a signed link.
  - More than 5,000 rows → `202` and a job.
  - GST reports exclude Account copies (BR-04) and group by GSTIN or place of supply and rate; HSN summary includes charges under their SAC codes.
- **Tally export** (job):
  - **Masters:** stock groups, units, stock items, party ledgers with opening balances.
  - **Vouchers:** Sales (Cash/Account per mapping), Credit Note, Receipt, Payment and Purchase.
  - **Increments:** `tally_export_item` records each exported document version. A first export is `Create`; an edited bill (new version) is `Alter`; a cancelled bill, purchase, payment or a bounced cheque is `Cancel` (FR-TI-03). Walk-in sales use the cash ledger.
  - The XML goes to Storage; `GET /jobs/{id}` returns the link.
- **Tally import:**
  - Upload a Tally masters XML or an Excel workbook (sheets Groups, Units, Items, Customers, Suppliers).
  - The preview stores the parsed data for 24 h with new/existing/error counts and issues (unknown units or godowns, bad GST rates, invalid GSTINs).
  - The commit (job, all-or-nothing) maps godowns and units, creates missing categories, units, items and parties, posts opening stock at the cutover date, and converts Tally's sign (Dr negative) to the app's.

## 10. Database

- **`supabase/migrations/20261010000000_init_schema.sql`** is **your `schema.sql`, verbatim**. Only its `BEGIN;`/`COMMIT;` lines were removed, because the Supabase CLI wraps each migration in a transaction.
- **`20261010000100_api_support.sql`** adds only what the API needs:

| Addition | Why |
|---|---|
| `river` schema + grants | River job tables (created by River's migrator as `billing_api`) |
| `shop_settings`: number prefixes, notification channels, Tally mapping, encrypted GSP credentials, `locked_until` | `/settings` endpoints, FR-ED-04 |
| `fn_next_doc_no` (replaced) | Uses the settings prefixes when a series starts |
| `fn_remove_cost` | Reverses the average-cost effect for purchase cancel/return |
| `fn_ref_no` | Document number for a (ref_type, ref_id), used by ledgers and stock history |
| `app_user.last_session_id` + "keep a Super Admin" trigger | Login audit; FR-AU-02 |
| `bill.einv_error`, `credit_note.refund / refund_payment_id / reject_reason / einv_error` | E-invoice errors; refunds on approval |
| `payment.bill_id`, cheque dates, cancel fields | Bill-screen payments; cheque life cycle; cancel |
| `purchase` cancel fields | Purchase cancel |
| `job`, `tally_import` tables | `GET /jobs/{id}`; Tally import preview |
| Indexes | Payments, allocations, returns, stock history, bill search (trigram) |
| Grants / RLS | `party_ledger`, `bill_revision` and `einvoice_log` are append-only for `billing_api`; RLS on new tables |

- **sqlc** generates `internal/db/dbq` from `db/queries`. The migrations create objects after `SET search_path = app`, so queries use unqualified names; at runtime the pool's `search_path` is `app,extensions`. Decimals map to `shopspring/decimal` (pgx-shopspring-decimal is registered on every connection).

## 11. Tests

| Test | What it covers |
|---|---|
| `internal/num` | Indian amount in words, rounding |
| `internal/billing` | Calculator: intra/inter-state, the API design's worked example, half-up rounding, bill discount spread, GST-inclusive rates, over-discount errors |
| `internal/db` | Migrations + seed apply; numbering with prefixes; stock-out blocking; average cost up and back down; last Super Admin; append-only history; browser roles locked out |
| `internal/server` `TestAPI` | 26 sequential scenarios through HTTP covering all 102 operations, each response validated against `openapi.yaml` (status documented and body matching its schema), ending with a check that stock balances equal the movement history |
| `internal/server` `TestPostmanCollection` | Runs the Postman collection with newman against a fresh server (122 requests, 0 failures) |

Run them with:

```powershell
cd api
go test -short ./...   # unit tests only
go test ./...          # everything (needs Docker; the Postman test also needs Node/npx)
```

The integration tests use a fake Supabase (JWKS, Auth admin, Storage, password sign-in) and set `TESTCONTAINERS_RYUK_DISABLED`, because Ryuk does not start on Docker Desktop for Windows.

## 12. Review points and open decisions

These are places where the documents leave a choice open, or where real-world behaviour should be confirmed.

| # | Topic | What the code does | To confirm |
|---|---|---|---|
| R1 | Cancel a bill with payments | Payments taken on the bill screen are cancelled (treated as refunded); receipts recorded later go back on account | Correct for cash refunds? |
| R2 | Reversal date | `fn_unpost_bill` (from schema.sql) dates reversals today; a March bill cancelled in April reverses in April | Accountant |
| R3 | Returns on an Account copy | Refused (409): returns are made against the source Cash bill, which carries the stock and ledger | OK? |
| R4 | Landed cost | Excludes GST (assumes input tax credit is claimed) | Accountant |
| R5 | Average cost | One average per item across locations. Per schema.sql, only purchases and opening stock change it; sale reversals and sales returns come back at their original cost without moving the average | OK? |
| R6 | Account copy and GST | Not e-invoiced and excluded from GST reports and sales, so the sale is reported once (BR-04). An independent Account bill is a full GST invoice | Accountant (doc R-2) |
| R7 | Edit inside the IRN window | Cancels the IRN and regenerates with the **same invoice number**, as FR-ED-03 says. Most IRPs reject a number that was used for a cancelled IRN | Confirm with the chosen GSP; may need a new number or a credit note instead |
| R8 | Credit limit | Hard block (422) with Super Admin override; the doc says "warn" (Should) | OK? |
| R9 | Cancel a Cash bill that has an Account copy | Blocked (409) until the copy is cancelled; the API doc says "warns" | OK? |
| R10 | Drafts and payments | Drafts can't carry payments; record them when finalizing or afterwards | OK? |
| R11 | Notification "read" | Role broadcasts share one read flag (all Super Admins) | OK for 1–2 Super Admins |
| R12 | Tally XML | Built to the Tally import format but not yet tried on a real Tally Prime company; masters are matched by name | Test with Tally |
| R13 | GSP provider | Only `mock` exists. A real provider is one more `Provider` implementation | O-07 |
| R14 | Not built | WhatsApp/email delivery of alerts (in-app only); nightly encrypted `pg_dump` (NFR-04, needs `pg_dump` in the image); the web app | Next steps |
| R15 | Settings before set-up | `GET /settings` returns an empty profile; bills, purchases and returns answer 409 until settings are saved | OK? |

### Statuses outside the spec

The code returns these, but `openapi.yaml` doesn't list them for the operation:
- **`GET` list endpoints:** 400 for malformed query parameters (e.g. an unknown `sort` field).
- **`GET /bills/{id}/pdf` and `/share`:** 409 while the bill is a draft, or while its e-invoice is pending or failed.
- **`GET /jobs/{id}`:** 404 for an unknown job.

## 13. Changes made to openapi.yaml

The contract tests found four places where the spec could not validate correct responses. The fixes are minimal and are the only edits to your spec; nothing else in it was changed.

1. **422 responses** of `POST /bills`, `PUT /bills/{id}` and `POST /bills/{id}/finalize`: `oneOf` → `anyOf`. `InsufficientStockProblem` and `CreditLimitProblem` only add optional fields to `Problem`, so every error body matched both and `oneOf` ("exactly one") always failed.
2. **`GET /reports/{key}` 200**: `oneOf` → `anyOf` for the same reason (the rows object has no required fields, so a `SignedUrl` matched both).
3. **`DashboardSummary.gross_margin_pct`**: its own pattern `^-?\d{1,6}(\.\d{1,2})?$`, because a margin can be negative (selling below cost) and the shared `Percent` pattern did not allow that.
4. **`Payment.party_id`**: nullable. `schema.sql` allows a null party for walk-in cash taken on a bill, and such receipts appear in `/payments`.
