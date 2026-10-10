# Billing & Inventory Management System

Web billing and inventory for a construction-material shop (1 shop, 2 godowns).

| Document | Role |
|---|---|
| [Billing_Inventory_Requirements_HLD_v2.1.docx](Billing_Inventory_Requirements_HLD_v2.1.docx) | Requirements, HLD and database design (content v2.2) |
| [Billing_API_Design_v1.docx](Billing_API_Design_v1.docx) | API conventions and flows |
| [openapi.yaml](openapi.yaml) | The REST contract (102 operations); the tests validate every response against it |
| [schema.sql](schema.sql) | The schema from the design; it is the first migration, verbatim |

| Folder | What | Host |
|---|---|---|
| `api/` | Go API + River job worker (chi, pgx, sqlc, River) | Railway |
| `supabase/` | Database migrations, seed, Supabase config | Supabase (Free dev / Pro prod) |
| `postman/` | Runnable Postman collection for every operation, environments, sample files | — |
| `web/` | React + Tailwind app (not created yet) | Vercel |

**Reviewing the logic?** Start with [api/README.md](api/README.md). It covers each flow step by step (bills, edits, cancels, payments, returns, purchases, stock, e-invoice, Tally), where each rule is enforced, the tests, and the open review points.

## Tools

Installed on this machine: Go, Git, Node LTS, Supabase CLI, sqlc and Docker Desktop. The Supabase CLI and sqlc live in `%LOCALAPPDATA%\Programs\devtools`, which is on the user PATH. Open a new terminal after installing so it picks up the new PATH.

## 1. Supabase projects (one-time, in the dashboard)

Create **two** projects (HLD 6.4): `billing-dev` (Free) and later `billing-prod` (Pro). Both should be owned by the shop owner's account, with the developer invited (O-10). Region: Mumbai (ap-south-1).

For each project:

1. **Link and push the schema** (from the repo root):
   ```powershell
   supabase login
   supabase link --project-ref <project-ref>
   supabase db push            # applies supabase/migrations/* (schema.sql, then the API additions)
   supabase config push        # applies auth settings from supabase/config.toml
   ```
2. **Set the API role's password.** Run this in the SQL editor and keep the password only in Railway:
   ```sql
   alter role billing_api with password '<strong-random-password>';
   ```
3. **Data API.** Under Settings → API → Exposed schemas, leave only `public` and `graphql_public`. Never add `app` or `river`.
4. **Auth.** Check these settings (`config push` sets most of them):
   - Sign-ups are off.
   - Email confirmations are on.
   - Minimum password length is 10, with mixed case and digits required.
   - Site URL and redirect URLs are the web app's domain.
5. **JWT signing keys.** Under Settings → JWT Keys, use asymmetric (ECC/RSA) signing keys. The API verifies tokens against `/auth/v1/.well-known/jwks.json`; legacy HS256-only projects have no public keys to verify against.
6. **Storage.** Create a **private** bucket named `documents` for invoice PDFs, the logo, imports and exports. `config.toml` defines it for the local stack only.
7. **First Super Admin.** Under Authentication → Users → Add user, create the owner with "auto confirm" ticked. Then run:
   ```sql
   insert into app.app_user (id, full_name, email, role)
   select id, 'Owner Name', email, 'super_admin' from auth.users where email = 'owner@example.com';
   ```
   Every later user is invited by the Super Admin through `POST /users`.
8. **Shop settings.** Sign in and call `PUT /settings` (shop name, address, state code, GSTIN) before the first bill. The state code decides between CGST/SGST and IGST.
9. **Production only:** under Database → Backups, confirm daily backups are on. The nightly `pg_dump` worker (NFR-04) is not built yet.

## 2. Run the API locally

```powershell
cd api
copy .env.example .env       # fill in DATABASE_URL, SUPABASE_URL, SUPABASE_SERVICE_ROLE_KEY, APP_ENCRYPTION_KEY
go run ./cmd/api             # API on :8080, worker in the same process
curl http://localhost:8080/api/v1/health
```

`APP_ENCRYPTION_KEY` is 32 random bytes, base64-encoded. It encrypts the GSP credentials; generate one with `openssl rand -base64 32`.

**Fully local option (needs Docker running):**
1. Run `supabase start` from the repo root, then `supabase db reset`, which applies the migrations and `seed.sql` (sample categories, items, parties and stock).
2. In Studio (http://127.0.0.1:54323), set the `billing_api` password and add the first Super Admin as in step 7 above.
3. Point `.env` at `127.0.0.1:54322` and `http://127.0.0.1:54321`, and use the local service-role key printed by `supabase start`.

## 3. Postman

1. Import `postman/billing-api.postman_collection.json` and one environment (`local` or `dev`).
2. Fill `supabase_url`, `supabase_anon_key`, `email` and `password` (a Super Admin).
3. Run **Auth (Supabase) → Sign in**, then the folders in order (the Collection Runner works).

Ids created along the way are stored in collection variables. File uploads use `postman/samples/*`. The collection is generated by `node postman/build-collection.mjs`, which fails if an operation in `openapi.yaml` is missing.

## 4. Tests

```powershell
cd api
go test -short ./...         # unit tests (calculator, amounts in words)
go test ./...                # + database tests, end-to-end API tests, and the Postman collection via newman
```

The end-to-end tests need Docker; the Postman test also needs Node (`npx`). They:
- start PostgreSQL 17 with the real migrations and a fake Supabase (JWKS, Auth admin, Storage),
- run the real server and job workers,
- check every response against `openapi.yaml`.

## 5. Deploy

- **Railway** (API):
  - Create a service from the GitHub repo and set its root directory to `/api`. It builds `api/Dockerfile`, and `railway.json` sets `/health` as the health check.
  - Set the variables from `api/.env.example`.
  - In production, set `APP_ENV=production` and limit `CORS_ALLOWED_ORIGINS` to the website domain.
- **Database changes:** only through new files in `supabase/migrations/`. Apply them with `supabase db push` to dev first, then to prod. Never edit a migration that has been pushed.
