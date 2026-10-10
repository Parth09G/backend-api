-- Source: schema.sql (companion to the HLD, v2.1 + api_idempotency), copied verbatim.
-- Only the BEGIN/COMMIT wrapper was removed: the Supabase CLI runs each migration in a transaction.
-- API-specific additions live in the next migration, not here.

-- =====================================================================
-- Billing & Inventory Management System — PostgreSQL schema  (v2.1, Supabase)
-- Construction material retail shop: 1 shop + 2 godowns, GST + e-invoice
-- Runs on Supabase (Free for development, Pro for production) or plain PostgreSQL 15+.
-- All tables live in a private schema "app" that is NOT exposed through Supabase's
-- Data API. Only the backend API (Railway) connects, using its own database role.
-- Logins are handled by Supabase Auth; app.app_user.id = auth.users.id.
-- Money: numeric(14,2)  Quantity: numeric(14,3)  Cost: numeric(14,4)
-- =====================================================================


CREATE SCHEMA IF NOT EXISTS extensions;
CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA extensions;   -- fast fuzzy item / customer search
CREATE SCHEMA app;
SET search_path = app, extensions;

-- ---------------------------------------------------------------------
-- Enumerations
-- ---------------------------------------------------------------------
CREATE TYPE user_role        AS ENUM ('super_admin', 'admin');
CREATE TYPE location_kind    AS ENUM ('shop', 'godown');
CREATE TYPE party_kind       AS ENUM ('customer', 'supplier', 'both');
CREATE TYPE bill_kind        AS ENUM ('cash', 'account');
CREATE TYPE doc_status       AS ENUM ('draft', 'active', 'cancelled');
CREATE TYPE return_status    AS ENUM ('pending', 'approved', 'rejected', 'cancelled');
CREATE TYPE pay_mode         AS ENUM ('cash', 'upi', 'cheque', 'bank_transfer');
CREATE TYPE pay_direction    AS ENUM ('received', 'paid');
CREATE TYPE cheque_state     AS ENUM ('pending', 'cleared', 'bounced');
CREATE TYPE charge_kind      AS ENUM ('labour', 'transport', 'other');
CREATE TYPE attr_type        AS ENUM ('text', 'number', 'select');
CREATE TYPE einv_state       AS ENUM ('not_required', 'pending', 'generated', 'failed', 'cancelled');
CREATE TYPE movement_kind    AS ENUM ('opening', 'purchase', 'purchase_return', 'sale', 'sale_reversal',
                                      'sales_return', 'transfer_out', 'transfer_in', 'adjustment');
CREATE TYPE adjust_reason    AS ENUM ('damage', 'breakage', 'wastage', 'count_correction', 'sample', 'other');
CREATE TYPE doc_kind         AS ENUM ('cash_bill', 'account_bill', 'credit_note', 'receipt', 'payment_voucher',
                                      'purchase', 'purchase_return', 'transfer', 'adjustment');

-- ---------------------------------------------------------------------
-- Users, settings, locations
-- ---------------------------------------------------------------------
CREATE TABLE app_user (                                      -- profile + role for a Supabase Auth user
  id              uuid        PRIMARY KEY,                   -- = auth.users.id (FK added below if auth exists)
  full_name       text        NOT NULL,
  email           text        NOT NULL UNIQUE,               -- login id in Supabase Auth
  mobile          text        UNIQUE,
  role            user_role   NOT NULL DEFAULT 'admin',
  is_active       boolean     NOT NULL DEFAULT true,
  last_login_at   timestamptz,
  created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE shop_settings (                                 -- single row
  id                    smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
  shop_name             text NOT NULL,
  address               text NOT NULL,
  state_code            char(2) NOT NULL,                    -- GST state code, e.g. '29'
  gstin                 char(15) NOT NULL,
  phone                 text,
  email                 text,
  logo_path             text,
  bank_name             text,
  bank_account_no       text,
  bank_ifsc             text,
  upi_id                text,
  invoice_terms         text,
  prices_include_gst    boolean NOT NULL DEFAULT false,
  default_charge_gst    numeric(5,2) NOT NULL DEFAULT 18,
  einvoice_enabled      boolean NOT NULL DEFAULT true,
  bill_edit_window_hrs  int NOT NULL DEFAULT 24,             -- edits after IRN only inside IRP cancel window
  updated_at            timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE location (
  id          smallint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name        text NOT NULL UNIQUE,
  kind        location_kind NOT NULL,
  address     text,
  is_default_billing boolean NOT NULL DEFAULT false,
  is_active   boolean NOT NULL DEFAULT true
);
CREATE UNIQUE INDEX one_default_billing_location ON location (is_default_billing) WHERE is_default_billing;

-- ---------------------------------------------------------------------
-- Catalogue: units, categories, category attributes, items
-- ---------------------------------------------------------------------
CREATE TABLE unit (
  id          int GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name        text NOT NULL UNIQUE,                          -- 'Bag', 'Kilogram', 'Brass'
  symbol      text NOT NULL UNIQUE,                          -- 'bag', 'kg', 'brass'
  uqc_code    text,                                          -- GST Unit Quantity Code, e.g. 'BAG','KGS'
  decimals    smallint NOT NULL DEFAULT 3 CHECK (decimals BETWEEN 0 AND 3)
);

CREATE TABLE category (
  id            int GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name          text NOT NULL UNIQUE,
  parent_id     int REFERENCES category(id),
  default_hsn   text,
  default_gst   numeric(5,2),
  description   text,
  is_active     boolean NOT NULL DEFAULT true,
  tally_group   text,                                        -- Tally stock group name for import/export
  created_at    timestamptz NOT NULL DEFAULT now()
);

-- Units allowed for items in a category (defined while onboarding the category)
CREATE TABLE category_unit (
  category_id   int NOT NULL REFERENCES category(id) ON DELETE CASCADE,
  unit_id       int NOT NULL REFERENCES unit(id),
  is_default    boolean NOT NULL DEFAULT false,
  PRIMARY KEY (category_id, unit_id)
);

-- Per-category item attributes: cement→brand; steel→brand,size; pipes→size,weight,shape …
CREATE TABLE category_attribute (
  id            int GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  category_id   int NOT NULL REFERENCES category(id) ON DELETE CASCADE,
  code          text NOT NULL,                               -- 'brand','size','weight','shape'
  label         text NOT NULL,
  data_type     attr_type NOT NULL DEFAULT 'text',
  options       text[],                                      -- for 'select'
  value_unit    text,                                        -- e.g. 'mm', 'kg/m', 'L'
  is_required   boolean NOT NULL DEFAULT false,
  part_of_name  boolean NOT NULL DEFAULT true,               -- used to build display name / duplicate key
  sort_order    smallint NOT NULL DEFAULT 0,
  UNIQUE (category_id, code)
);

CREATE TABLE item (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  category_id     int NOT NULL REFERENCES category(id),
  name            text NOT NULL,
  attributes      jsonb NOT NULL DEFAULT '{}'::jsonb,        -- {"brand":"UltraTech","size":"12 mm"}
  sku             text UNIQUE,
  hsn_code        text NOT NULL,
  gst_rate        numeric(5,2) NOT NULL CHECK (gst_rate IN (0, 0.25, 3, 5, 12, 18, 28, 40)),
  base_unit_id    int NOT NULL REFERENCES unit(id),
  reorder_level   numeric(14,3) NOT NULL DEFAULT 0,          -- total across locations
  avg_cost        numeric(14,4) NOT NULL DEFAULT 0,          -- weighted average per base unit
  last_purchase_rate numeric(14,4),
  is_active       boolean NOT NULL DEFAULT true,
  tally_name      text,
  created_by      uuid REFERENCES app_user(id),
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (category_id, name)
);
CREATE INDEX item_name_trgm ON item USING gin (name extensions.gin_trgm_ops);
CREATE INDEX item_attr_gin  ON item USING gin (attributes);

-- Alternate units for an item with conversion to base unit (1 tonne = 1000 kg)
CREATE TABLE item_unit (
  item_id         bigint NOT NULL REFERENCES item(id) ON DELETE CASCADE,
  unit_id         int NOT NULL REFERENCES unit(id),
  factor_to_base  numeric(14,6) NOT NULL CHECK (factor_to_base > 0),
  PRIMARY KEY (item_id, unit_id)
);

-- ---------------------------------------------------------------------
-- Stock: balance per item per location + immutable movement ledger
-- ---------------------------------------------------------------------
CREATE TABLE stock_balance (
  item_id      bigint   NOT NULL REFERENCES item(id),
  location_id  smallint NOT NULL REFERENCES location(id),
  qty          numeric(14,3) NOT NULL DEFAULT 0 CHECK (qty >= 0),   -- negative stock impossible
  updated_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (item_id, location_id)
);

CREATE TABLE stock_movement (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  item_id       bigint   NOT NULL REFERENCES item(id),
  location_id   smallint NOT NULL REFERENCES location(id),
  kind          movement_kind NOT NULL,
  qty_in        numeric(14,3) NOT NULL DEFAULT 0 CHECK (qty_in  >= 0),
  qty_out       numeric(14,3) NOT NULL DEFAULT 0 CHECK (qty_out >= 0),
  unit_cost     numeric(14,4),                               -- cost per base unit at time of movement
  ref_type      doc_kind,
  ref_id        bigint,
  ref_line_id   bigint,
  note          text,
  created_by    uuid REFERENCES app_user(id),
  created_at    timestamptz NOT NULL DEFAULT now(),
  CHECK ((qty_in > 0) <> (qty_out > 0))
);
CREATE INDEX stock_movement_item_idx ON stock_movement (item_id, created_at);
CREATE INDEX stock_movement_ref_idx  ON stock_movement (ref_type, ref_id);

-- ---------------------------------------------------------------------
-- Parties (customers & suppliers/dealers) and ledger
-- ---------------------------------------------------------------------
CREATE TABLE party (
  id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  kind              party_kind NOT NULL,
  name              text NOT NULL,
  mobile            text,
  alt_phone         text,
  gstin             char(15) CHECK (gstin ~ '^[0-9]{2}[A-Z]{5}[0-9]{4}[A-Z][0-9A-Z]Z[0-9A-Z]$'),
  state_code        char(2),
  billing_address   text,
  pincode           char(6),
  credit_limit      numeric(14,2),
  opening_balance   numeric(14,2) NOT NULL DEFAULT 0,        -- +ve = party owes shop (Dr)
  opening_date      date,
  tally_ledger      text,
  notes             text,
  is_active         boolean NOT NULL DEFAULT true,
  created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX party_name_trgm ON party USING gin (name extensions.gin_trgm_ops);
CREATE UNIQUE INDEX party_gstin_uq ON party (gstin) WHERE gstin IS NOT NULL;

CREATE TABLE party_site (
  id        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  party_id  bigint NOT NULL REFERENCES party(id) ON DELETE CASCADE,
  label     text NOT NULL,
  address   text NOT NULL
);

-- Double-entry style party ledger: one row per posting. Balance = Σ(debit − credit)
CREATE TABLE party_ledger (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  party_id    bigint NOT NULL REFERENCES party(id),
  entry_date  date   NOT NULL,
  ref_type    doc_kind NOT NULL,
  ref_id      bigint NOT NULL,
  debit       numeric(14,2) NOT NULL DEFAULT 0 CHECK (debit  >= 0),
  credit      numeric(14,2) NOT NULL DEFAULT 0 CHECK (credit >= 0),
  narration   text,
  is_reversal boolean NOT NULL DEFAULT false,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX party_ledger_party_idx ON party_ledger (party_id, entry_date);
CREATE INDEX party_ledger_ref_idx   ON party_ledger (ref_type, ref_id);

-- ---------------------------------------------------------------------
-- Document numbering (per type per financial year, gap-free, concurrency-safe)
-- ---------------------------------------------------------------------
CREATE TABLE doc_series (
  doc_type   doc_kind NOT NULL,
  fy         char(5)  NOT NULL,                              -- '26-27'
  prefix     text     NOT NULL,                              -- 'C/26-27/'
  next_no    int      NOT NULL DEFAULT 1,
  PRIMARY KEY (doc_type, fy),
  CHECK (length(prefix) <= 10)                               -- keeps full number ≤ 16 chars
);

-- ---------------------------------------------------------------------
-- Bills (Cash & Account), lines, charges, revisions
-- ---------------------------------------------------------------------
CREATE TABLE bill (
  id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  bill_type          bill_kind  NOT NULL,
  bill_no            text,                                   -- null while draft
  fy                 char(5),
  bill_date          date       NOT NULL DEFAULT current_date,
  status             doc_status NOT NULL DEFAULT 'draft',
  version            int        NOT NULL DEFAULT 1,
  source_bill_id     bigint REFERENCES bill(id),             -- Account bill copied from a Cash bill
  party_id           bigint REFERENCES party(id),            -- null = walk-in (Cash bills only)
  -- snapshot of customer details as printed
  cust_name          text NOT NULL,
  cust_gstin         char(15),
  cust_address       text,
  cust_mobile        text,
  site_address       text,
  place_of_supply    char(2) NOT NULL,
  is_interstate      boolean NOT NULL,
  vehicle_no         text,
  -- flags derived at posting time
  affects_stock      boolean NOT NULL,                       -- true for cash, false for account
  posts_to_ledger    boolean NOT NULL DEFAULT true,          -- false for an Account copy of a Cash bill
  -- totals
  items_taxable      numeric(14,2) NOT NULL DEFAULT 0,
  charges_taxable    numeric(14,2) NOT NULL DEFAULT 0,
  bill_discount      numeric(14,2) NOT NULL DEFAULT 0,
  taxable_total      numeric(14,2) NOT NULL DEFAULT 0,
  cgst               numeric(14,2) NOT NULL DEFAULT 0,
  sgst               numeric(14,2) NOT NULL DEFAULT 0,
  igst               numeric(14,2) NOT NULL DEFAULT 0,
  round_off          numeric(6,2)  NOT NULL DEFAULT 0,
  grand_total        numeric(14,2) NOT NULL DEFAULT 0,
  amount_paid        numeric(14,2) NOT NULL DEFAULT 0,
  balance_due        numeric(14,2) GENERATED ALWAYS AS (grand_total - amount_paid) STORED,
  cost_total         numeric(14,2) NOT NULL DEFAULT 0,       -- for profit
  -- e-invoice / e-way bill
  einv_status        einv_state NOT NULL DEFAULT 'not_required',
  irn                char(64),
  ack_no             text,
  ack_date           timestamptz,
  signed_qr          text,
  ewb_no             text,
  ewb_valid_until    timestamptz,
  -- lifecycle
  notes              text,
  cancel_reason      text,
  cancelled_by       uuid REFERENCES app_user(id),
  cancelled_at       timestamptz,
  created_by         uuid NOT NULL REFERENCES app_user(id),
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_by         uuid REFERENCES app_user(id),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  UNIQUE (bill_type, fy, bill_no),
  CHECK (status = 'draft' OR bill_no IS NOT NULL),
  CHECK (bill_type = 'cash' OR party_id IS NOT NULL),       -- Account bills need a named customer
  CHECK (source_bill_id IS NULL OR bill_type = 'account'),
  CHECK (amount_paid <= grand_total OR status = 'draft')
);
CREATE INDEX bill_date_idx   ON bill (bill_date);
CREATE INDEX bill_party_idx  ON bill (party_id);
CREATE INDEX bill_source_idx ON bill (source_bill_id);
CREATE INDEX bill_status_idx ON bill (bill_type, status);

CREATE TABLE bill_line (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  bill_id         bigint   NOT NULL REFERENCES bill(id) ON DELETE CASCADE,
  line_no         smallint NOT NULL,
  item_id         bigint   NOT NULL REFERENCES item(id),
  location_id     smallint NOT NULL REFERENCES location(id), -- shop or godown the goods leave from
  description     text     NOT NULL,                         -- item name + attributes as printed
  hsn_code        text     NOT NULL,
  qty             numeric(14,3) NOT NULL CHECK (qty > 0),
  unit_id         int      NOT NULL REFERENCES unit(id),
  factor_to_base  numeric(14,6) NOT NULL DEFAULT 1,
  base_qty        numeric(14,3) GENERATED ALWAYS AS (round(qty * factor_to_base, 3)) STORED,
  rate            numeric(14,2) NOT NULL CHECK (rate >= 0),  -- entered on every bill (prices vary)
  discount_pct    numeric(5,2)  NOT NULL DEFAULT 0,
  discount_amt    numeric(14,2) NOT NULL DEFAULT 0,
  taxable_value   numeric(14,2) NOT NULL,
  gst_rate        numeric(5,2)  NOT NULL,
  cgst            numeric(14,2) NOT NULL DEFAULT 0,
  sgst            numeric(14,2) NOT NULL DEFAULT 0,
  igst            numeric(14,2) NOT NULL DEFAULT 0,
  line_total      numeric(14,2) NOT NULL,
  unit_cost       numeric(14,4),                             -- avg cost at sale, for profit
  UNIQUE (bill_id, line_no)
);
CREATE INDEX bill_line_item_idx ON bill_line (item_id);

CREATE TABLE bill_charge (
  id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  bill_id        bigint NOT NULL REFERENCES bill(id) ON DELETE CASCADE,
  kind           charge_kind NOT NULL,                       -- labour / transport / other
  description    text NOT NULL,
  sac_code       text,
  amount         numeric(14,2) NOT NULL CHECK (amount >= 0),
  gst_rate       numeric(5,2)  NOT NULL DEFAULT 18,
  cgst           numeric(14,2) NOT NULL DEFAULT 0,
  sgst           numeric(14,2) NOT NULL DEFAULT 0,
  igst           numeric(14,2) NOT NULL DEFAULT 0
);

-- Full snapshot of a bill before every edit (version history)
CREATE TABLE bill_revision (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  bill_id     bigint NOT NULL REFERENCES bill(id),
  version     int    NOT NULL,
  snapshot    jsonb  NOT NULL,                               -- header + lines + charges + payments
  reason      text   NOT NULL,
  edited_by   uuid NOT NULL REFERENCES app_user(id),
  edited_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (bill_id, version)
);

-- ---------------------------------------------------------------------
-- Payments (received from customers / paid to dealers) and allocation
-- ---------------------------------------------------------------------
CREATE TABLE payment (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  direction     pay_direction NOT NULL,
  doc_no        text NOT NULL,                               -- receipt / voucher number
  fy            char(5) NOT NULL,
  party_id      bigint REFERENCES party(id),                 -- null only for walk-in cash at billing
  pay_date      date   NOT NULL DEFAULT current_date,
  mode          pay_mode NOT NULL,
  amount        numeric(14,2) NOT NULL CHECK (amount > 0),
  reference     text,                                        -- UPI ref / UTR
  cheque_no     text,
  cheque_date   date,
  bank_name     text,
  cheque_status cheque_state,
  status        doc_status NOT NULL DEFAULT 'active',
  notes         text,
  created_by    uuid NOT NULL REFERENCES app_user(id),
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (direction, fy, doc_no),
  CHECK (mode <> 'cheque' OR cheque_no IS NOT NULL)
);

CREATE TABLE payment_allocation (
  payment_id   bigint NOT NULL REFERENCES payment(id) ON DELETE CASCADE,
  ref_type     doc_kind NOT NULL,                            -- cash_bill / account_bill / purchase
  ref_id       bigint NOT NULL,
  amount       numeric(14,2) NOT NULL CHECK (amount > 0),
  PRIMARY KEY (payment_id, ref_type, ref_id)
);

-- ---------------------------------------------------------------------
-- Sales returns / credit notes
-- ---------------------------------------------------------------------
CREATE TABLE credit_note (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  cn_no           text,
  fy              char(5),
  cn_date         date NOT NULL DEFAULT current_date,
  bill_id         bigint NOT NULL REFERENCES bill(id),
  party_id        bigint REFERENCES party(id),
  status          return_status NOT NULL DEFAULT 'pending',  -- Super Admin approves
  reason          text NOT NULL,
  taxable_total   numeric(14,2) NOT NULL DEFAULT 0,
  cgst            numeric(14,2) NOT NULL DEFAULT 0,
  sgst            numeric(14,2) NOT NULL DEFAULT 0,
  igst            numeric(14,2) NOT NULL DEFAULT 0,
  grand_total     numeric(14,2) NOT NULL DEFAULT 0,
  settlement      text CHECK (settlement IN ('refund', 'adjust_ledger')),
  einv_status     einv_state NOT NULL DEFAULT 'not_required',
  irn             char(64),
  ack_no          text,
  ack_date        timestamptz,
  signed_qr       text,
  created_by      uuid NOT NULL REFERENCES app_user(id),
  created_at      timestamptz NOT NULL DEFAULT now(),
  approved_by     uuid REFERENCES app_user(id),
  approved_at     timestamptz,
  UNIQUE (fy, cn_no)
);

CREATE TABLE credit_note_line (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  credit_note_id  bigint NOT NULL REFERENCES credit_note(id) ON DELETE CASCADE,
  bill_line_id    bigint NOT NULL REFERENCES bill_line(id),
  item_id         bigint NOT NULL REFERENCES item(id),
  location_id     smallint NOT NULL REFERENCES location(id), -- where returned goods are stored
  qty             numeric(14,3) NOT NULL CHECK (qty > 0),
  unit_id         int NOT NULL REFERENCES unit(id),
  factor_to_base  numeric(14,6) NOT NULL DEFAULT 1,
  base_qty        numeric(14,3) GENERATED ALWAYS AS (round(qty * factor_to_base, 3)) STORED,
  rate            numeric(14,2) NOT NULL,
  taxable_value   numeric(14,2) NOT NULL,
  gst_rate        numeric(5,2)  NOT NULL,
  cgst            numeric(14,2) NOT NULL DEFAULT 0,
  sgst            numeric(14,2) NOT NULL DEFAULT 0,
  igst            numeric(14,2) NOT NULL DEFAULT 0,
  restock         boolean NOT NULL DEFAULT true              -- false = damaged, not back to stock
);

-- ---------------------------------------------------------------------
-- Purchases from dealers (goods received into a chosen godown/shop)
-- ---------------------------------------------------------------------
CREATE TABLE purchase (
  id                   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  doc_no               text NOT NULL,                        -- internal GRN number
  fy                   char(5) NOT NULL,
  supplier_id          bigint NOT NULL REFERENCES party(id),
  supplier_invoice_no  text NOT NULL,
  supplier_invoice_dt  date NOT NULL,
  received_date        date NOT NULL DEFAULT current_date,
  location_id          smallint NOT NULL REFERENCES location(id),  -- default godown for lines
  is_interstate        boolean NOT NULL DEFAULT false,
  taxable_total        numeric(14,2) NOT NULL DEFAULT 0,
  freight              numeric(14,2) NOT NULL DEFAULT 0,
  other_charges        numeric(14,2) NOT NULL DEFAULT 0,
  cgst                 numeric(14,2) NOT NULL DEFAULT 0,
  sgst                 numeric(14,2) NOT NULL DEFAULT 0,
  igst                 numeric(14,2) NOT NULL DEFAULT 0,
  round_off            numeric(6,2)  NOT NULL DEFAULT 0,
  grand_total          numeric(14,2) NOT NULL DEFAULT 0,
  status               doc_status NOT NULL DEFAULT 'active',
  notes                text,
  created_by           uuid NOT NULL REFERENCES app_user(id),
  created_at           timestamptz NOT NULL DEFAULT now(),
  UNIQUE (fy, doc_no),
  UNIQUE (supplier_id, supplier_invoice_no)
);

CREATE TABLE purchase_line (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  purchase_id     bigint NOT NULL REFERENCES purchase(id) ON DELETE CASCADE,
  item_id         bigint NOT NULL REFERENCES item(id),
  location_id     smallint NOT NULL REFERENCES location(id),
  qty             numeric(14,3) NOT NULL CHECK (qty > 0),
  unit_id         int NOT NULL REFERENCES unit(id),
  factor_to_base  numeric(14,6) NOT NULL DEFAULT 1,
  base_qty        numeric(14,3) GENERATED ALWAYS AS (round(qty * factor_to_base, 3)) STORED,
  rate            numeric(14,4) NOT NULL CHECK (rate >= 0),
  discount_amt    numeric(14,2) NOT NULL DEFAULT 0,
  taxable_value   numeric(14,2) NOT NULL,
  gst_rate        numeric(5,2)  NOT NULL,
  cgst            numeric(14,2) NOT NULL DEFAULT 0,
  sgst            numeric(14,2) NOT NULL DEFAULT 0,
  igst            numeric(14,2) NOT NULL DEFAULT 0,
  landed_unit_cost numeric(14,4)                             -- incl. apportioned freight, per base unit
);

CREATE TABLE purchase_return (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  doc_no        text NOT NULL,
  fy            char(5) NOT NULL,
  purchase_id   bigint NOT NULL REFERENCES purchase(id),
  return_date   date NOT NULL DEFAULT current_date,
  reason        text NOT NULL,
  grand_total   numeric(14,2) NOT NULL DEFAULT 0,
  created_by    uuid NOT NULL REFERENCES app_user(id),
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (fy, doc_no)
);

CREATE TABLE purchase_return_line (
  id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  purchase_return_id  bigint NOT NULL REFERENCES purchase_return(id) ON DELETE CASCADE,
  purchase_line_id    bigint NOT NULL REFERENCES purchase_line(id),
  item_id             bigint NOT NULL REFERENCES item(id),
  location_id         smallint NOT NULL REFERENCES location(id),
  base_qty            numeric(14,3) NOT NULL CHECK (base_qty > 0),
  rate                numeric(14,4) NOT NULL,
  amount              numeric(14,2) NOT NULL
);

-- ---------------------------------------------------------------------
-- Stock transfers (shop ⇄ godowns) and adjustments
-- ---------------------------------------------------------------------
CREATE TABLE stock_transfer (
  id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  doc_no            text NOT NULL,
  fy                char(5) NOT NULL,
  transfer_date     date NOT NULL DEFAULT current_date,
  from_location_id  smallint NOT NULL REFERENCES location(id),
  to_location_id    smallint NOT NULL REFERENCES location(id),
  vehicle_no        text,
  notes             text,
  created_by        uuid NOT NULL REFERENCES app_user(id),
  created_at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE (fy, doc_no),
  CHECK (from_location_id <> to_location_id)
);

CREATE TABLE stock_transfer_line (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  transfer_id   bigint NOT NULL REFERENCES stock_transfer(id) ON DELETE CASCADE,
  item_id       bigint NOT NULL REFERENCES item(id),
  base_qty      numeric(14,3) NOT NULL CHECK (base_qty > 0)
);

CREATE TABLE stock_adjustment (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  doc_no        text NOT NULL,
  fy            char(5) NOT NULL,
  adj_date      date NOT NULL DEFAULT current_date,
  location_id   smallint NOT NULL REFERENCES location(id),
  reason        adjust_reason NOT NULL,
  notes         text,
  created_by    uuid NOT NULL REFERENCES app_user(id),
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (fy, doc_no)
);

CREATE TABLE stock_adjustment_line (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  adjustment_id   bigint NOT NULL REFERENCES stock_adjustment(id) ON DELETE CASCADE,
  item_id         bigint NOT NULL REFERENCES item(id),
  qty_change      numeric(14,3) NOT NULL CHECK (qty_change <> 0)   -- +in / −out, base unit
);

-- ---------------------------------------------------------------------
-- Integrations, notifications, audit
-- ---------------------------------------------------------------------
CREATE TABLE einvoice_log (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  ref_type    doc_kind NOT NULL,
  ref_id      bigint NOT NULL,
  action      text NOT NULL CHECK (action IN ('generate_irn', 'cancel_irn', 'generate_ewb', 'cancel_ewb')),
  request     jsonb,
  response    jsonb,
  success     boolean NOT NULL,
  error_code  text,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX einvoice_log_ref_idx ON einvoice_log (ref_type, ref_id);

CREATE TABLE tally_sync (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  direction     text NOT NULL CHECK (direction IN ('import', 'export')),
  scope         text NOT NULL,                               -- 'masters','opening','sales','purchases','receipts'
  period_from   date,
  period_to     date,
  file_path     text,
  record_count  int,
  summary       jsonb,                                       -- created / skipped / failed
  status        text NOT NULL DEFAULT 'completed',
  created_by    uuid REFERENCES app_user(id),
  created_at    timestamptz NOT NULL DEFAULT now()
);
-- Tracks which documents have been exported to Tally so they are not sent twice
CREATE TABLE tally_export_item (
  sync_id     bigint NOT NULL REFERENCES tally_sync(id),
  ref_type    doc_kind NOT NULL,
  ref_id      bigint NOT NULL,
  doc_version int NOT NULL DEFAULT 1,
  PRIMARY KEY (ref_type, ref_id, doc_version)
);

CREATE TABLE notification (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  for_role    user_role,                                     -- broadcast to a role …
  user_id     uuid REFERENCES app_user(id),                -- … or to one user
  kind        text NOT NULL,                                 -- 'stock_short','low_stock','return_pending','einv_failed','backup_failed'
  title       text NOT NULL,
  body        text,
  ref_type    doc_kind,
  ref_id      bigint,
  item_id     bigint REFERENCES item(id),
  is_read     boolean NOT NULL DEFAULT false,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX notification_unread_idx ON notification (is_read, created_at DESC);

CREATE TABLE audit_log (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id     uuid REFERENCES app_user(id),
  action      text NOT NULL,                                 -- create/update/cancel/delete/login/login_failed/export
  entity      text NOT NULL,
  entity_id   bigint,
  before_data jsonb,
  after_data  jsonb,
  ip_address  inet,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_entity_idx ON audit_log (entity, entity_id);
CREATE INDEX audit_time_idx   ON audit_log (created_at);
-- audit log is append-only
CREATE RULE audit_no_update AS ON UPDATE TO audit_log DO INSTEAD NOTHING;
CREATE RULE audit_no_delete AS ON DELETE TO audit_log DO INSTEAD NOTHING;

-- Idempotency keys: a retried POST (same user + key) returns the stored response instead of creating a duplicate
CREATE TABLE api_idempotency (
  user_id       uuid   NOT NULL REFERENCES app_user(id),
  key           uuid   NOT NULL,
  method        text   NOT NULL,
  path          text   NOT NULL,
  request_hash  bytea  NOT NULL,                             -- sha256 of the request body
  status_code   int,                                         -- null while the first request is still running
  response      jsonb,
  created_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, key)
);
CREATE INDEX api_idempotency_age_idx ON api_idempotency (created_at);   -- worker deletes rows older than 7 days

-- =====================================================================
-- Functions: numbering and stock (called by the API inside one transaction)
-- =====================================================================

-- Indian financial year label for a date: 2026-10-10 → '26-27'
CREATE FUNCTION fn_fy(d date) RETURNS char(5) LANGUAGE sql IMMUTABLE
  SET search_path = app, extensions AS $$
  SELECT CASE WHEN extract(month FROM d) >= 4
              THEN to_char(d, 'YY') || '-' || to_char(d + interval '1 year', 'YY')
              ELSE to_char(d - interval '1 year', 'YY') || '-' || to_char(d, 'YY') END;
$$;

-- Next document number; row lock makes it gap-free and safe under concurrency
CREATE FUNCTION fn_next_doc_no(p_type doc_kind, p_date date) RETURNS text LANGUAGE plpgsql
  SET search_path = app, extensions AS $$
DECLARE v_fy char(5) := fn_fy(p_date); v_prefix text; v_no int;
BEGIN
  INSERT INTO doc_series (doc_type, fy, prefix)
  VALUES (p_type, v_fy,
          CASE p_type WHEN 'cash_bill' THEN 'C' WHEN 'account_bill' THEN 'A' WHEN 'credit_note' THEN 'CN'
                      WHEN 'receipt' THEN 'R' WHEN 'payment_voucher' THEN 'PV' WHEN 'purchase' THEN 'P'
                      WHEN 'purchase_return' THEN 'PR' WHEN 'transfer' THEN 'T' ELSE 'ADJ' END
          || '/' || v_fy || '/')
  ON CONFLICT DO NOTHING;
  UPDATE doc_series SET next_no = next_no + 1
   WHERE doc_type = p_type AND fy = v_fy
  RETURNING prefix, next_no - 1 INTO v_prefix, v_no;
  RETURN v_prefix || lpad(v_no::text, 4, '0');
END $$;

-- Stock IN: increases balance, updates weighted average cost, writes movement
CREATE FUNCTION fn_stock_in(p_item bigint, p_loc smallint, p_qty numeric, p_cost numeric,
                            p_kind movement_kind, p_ref_type doc_kind, p_ref_id bigint,
                            p_ref_line bigint, p_user uuid) RETURNS void LANGUAGE plpgsql
  SET search_path = app, extensions AS $$
DECLARE v_total numeric; v_avg numeric;
BEGIN
  IF p_qty <= 0 THEN RAISE EXCEPTION 'quantity must be positive'; END IF;
  SELECT avg_cost INTO v_avg FROM item WHERE id = p_item FOR UPDATE;
  SELECT coalesce(sum(qty), 0) INTO v_total FROM stock_balance WHERE item_id = p_item;
  IF p_cost IS NOT NULL AND p_kind IN ('purchase', 'opening') THEN
    UPDATE item SET avg_cost = round((v_total * v_avg + p_qty * p_cost) / (v_total + p_qty), 4),
                    last_purchase_rate = CASE WHEN p_kind = 'purchase' THEN p_cost ELSE last_purchase_rate END,
                    updated_at = now()
     WHERE id = p_item;
  END IF;
  INSERT INTO stock_balance (item_id, location_id, qty) VALUES (p_item, p_loc, p_qty)
  ON CONFLICT (item_id, location_id) DO UPDATE SET qty = stock_balance.qty + EXCLUDED.qty, updated_at = now();
  INSERT INTO stock_movement (item_id, location_id, kind, qty_in, unit_cost, ref_type, ref_id, ref_line_id, created_by)
  VALUES (p_item, p_loc, p_kind, p_qty, coalesce(p_cost, v_avg), p_ref_type, p_ref_id, p_ref_line, p_user);
END $$;

-- Stock OUT: atomic conditional decrement; raises and alerts Super Admin if short
CREATE FUNCTION fn_stock_out(p_item bigint, p_loc smallint, p_qty numeric,
                             p_kind movement_kind, p_ref_type doc_kind, p_ref_id bigint,
                             p_ref_line bigint, p_user uuid) RETURNS numeric LANGUAGE plpgsql
  SET search_path = app, extensions AS $$
DECLARE v_avg numeric; v_have numeric; v_name text; v_locname text;
BEGIN
  IF p_qty <= 0 THEN RAISE EXCEPTION 'quantity must be positive'; END IF;
  UPDATE stock_balance SET qty = qty - p_qty, updated_at = now()
   WHERE item_id = p_item AND location_id = p_loc AND qty >= p_qty;
  IF NOT FOUND THEN
    SELECT coalesce((SELECT qty FROM stock_balance WHERE item_id = p_item AND location_id = p_loc), 0) INTO v_have;
    SELECT name INTO v_name FROM item WHERE id = p_item;
    SELECT name INTO v_locname FROM location WHERE id = p_loc;
    RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'INSUFFICIENT_STOCK',
      DETAIL = format('%s at %s: available %s, required %s', v_name, v_locname, v_have, p_qty),
      HINT = json_build_object('item_id', p_item, 'location_id', p_loc, 'available', v_have, 'required', p_qty)::text;
  END IF;
  SELECT avg_cost INTO v_avg FROM item WHERE id = p_item;
  INSERT INTO stock_movement (item_id, location_id, kind, qty_out, unit_cost, ref_type, ref_id, ref_line_id, created_by)
  VALUES (p_item, p_loc, p_kind, p_qty, v_avg, p_ref_type, p_ref_id, p_ref_line, p_user);
  RETURN v_avg;
END $$;

-- Shortage alert: written by the API in a separate transaction after a blocked bill
CREATE FUNCTION fn_alert_shortage(p_item bigint, p_loc smallint, p_required numeric, p_user uuid)
RETURNS void LANGUAGE sql
  SET search_path = app, extensions AS $$
  INSERT INTO notification (for_role, kind, title, body, item_id)
  SELECT 'super_admin', 'stock_short', 'Bill blocked – reorder ' || i.name,
         format('Required %s %s at %s, available %s. Raised by %s.',
                p_required, u.symbol, l.name,
                coalesce((SELECT qty FROM stock_balance WHERE item_id = p_item AND location_id = p_loc), 0),
                (SELECT full_name FROM app_user WHERE id = p_user)),
         p_item
    FROM item i JOIN unit u ON u.id = i.base_unit_id, location l
   WHERE i.id = p_item AND l.id = p_loc;
$$;

-- Post an active bill: stock (cash only) + ledger (unless it is an Account copy of a Cash bill)
CREATE FUNCTION fn_post_bill(p_bill bigint, p_user uuid) RETURNS void LANGUAGE plpgsql
  SET search_path = app, extensions AS $$
DECLARE b bill; l record; v_cost numeric; v_cost_total numeric := 0;
        v_ref doc_kind;
BEGIN
  SELECT * INTO b FROM bill WHERE id = p_bill FOR UPDATE;
  IF b.status <> 'active' THEN RAISE EXCEPTION 'bill % is not active', p_bill; END IF;
  v_ref := CASE b.bill_type WHEN 'cash' THEN 'cash_bill' ELSE 'account_bill' END;
  IF b.affects_stock THEN
    FOR l IN SELECT * FROM bill_line WHERE bill_id = p_bill ORDER BY item_id, location_id LOOP  -- fixed order avoids deadlocks
      v_cost := fn_stock_out(l.item_id, l.location_id, l.base_qty, 'sale', v_ref, p_bill, l.id, p_user);
      UPDATE bill_line SET unit_cost = v_cost WHERE id = l.id;
      v_cost_total := v_cost_total + round(v_cost * l.base_qty, 2);
    END LOOP;
  ELSE
    SELECT coalesce(sum(round(i.avg_cost * bl.base_qty, 2)), 0) INTO v_cost_total
      FROM bill_line bl JOIN item i ON i.id = bl.item_id WHERE bl.bill_id = p_bill;
  END IF;
  UPDATE bill SET cost_total = v_cost_total WHERE id = p_bill;
  IF b.posts_to_ledger AND b.party_id IS NOT NULL THEN
    INSERT INTO party_ledger (party_id, entry_date, ref_type, ref_id, debit, narration)
    VALUES (b.party_id, b.bill_date, v_ref, p_bill, b.grand_total, 'Sale ' || b.bill_no);
  END IF;
END $$;

-- Reverse a posted bill (used by cancel and by edit-before-repost)
CREATE FUNCTION fn_unpost_bill(p_bill bigint, p_user uuid) RETURNS void LANGUAGE plpgsql
  SET search_path = app, extensions AS $$
DECLARE b bill; l record; v_ref doc_kind;
BEGIN
  SELECT * INTO b FROM bill WHERE id = p_bill FOR UPDATE;
  v_ref := CASE b.bill_type WHEN 'cash' THEN 'cash_bill' ELSE 'account_bill' END;
  IF b.affects_stock THEN
    FOR l IN SELECT * FROM bill_line WHERE bill_id = p_bill ORDER BY item_id, location_id LOOP
      PERFORM fn_stock_in(l.item_id, l.location_id, l.base_qty, l.unit_cost, 'sale_reversal', v_ref, p_bill, l.id, p_user);
    END LOOP;
  END IF;
  IF b.posts_to_ledger AND b.party_id IS NOT NULL THEN
    INSERT INTO party_ledger (party_id, entry_date, ref_type, ref_id, credit, narration, is_reversal)
    VALUES (b.party_id, current_date, v_ref, p_bill, b.grand_total, 'Reversal ' || b.bill_no, true);
  END IF;
END $$;

-- =====================================================================
-- Views for dashboard and reports
-- =====================================================================
CREATE VIEW v_item_stock AS
SELECT i.id AS item_id, i.name, c.name AS category, i.attributes, u.symbol AS unit,
       coalesce(sum(sb.qty), 0)                       AS total_qty,
       jsonb_object_agg(l.name, coalesce(sb.qty, 0))  AS qty_by_location,
       i.reorder_level, i.avg_cost,
       round(coalesce(sum(sb.qty), 0) * i.avg_cost, 2) AS stock_value,
       CASE WHEN coalesce(sum(sb.qty), 0) = 0 THEN 'out'
            WHEN coalesce(sum(sb.qty), 0) <= i.reorder_level THEN 'low' ELSE 'ok' END AS status
  FROM item i
  JOIN category c ON c.id = i.category_id
  JOIN unit u ON u.id = i.base_unit_id
  CROSS JOIN location l
  LEFT JOIN stock_balance sb ON sb.item_id = i.id AND sb.location_id = l.id
 WHERE i.is_active AND l.is_active
 GROUP BY i.id, c.name, u.symbol;

CREATE VIEW v_party_balance AS
SELECT p.id AS party_id, p.name, p.kind,
       p.opening_balance + coalesce(sum(pl.debit - pl.credit), 0) AS balance
  FROM party p LEFT JOIN party_ledger pl ON pl.party_id = p.id
 GROUP BY p.id;

-- Sales = active bills that post to the ledger (avoids double counting Account copies)
CREATE VIEW v_sales AS
SELECT b.id, b.bill_type, b.bill_no, b.bill_date, b.party_id, b.cust_name, b.cust_gstin,
       b.taxable_total, b.cgst, b.sgst, b.igst, b.grand_total, b.cost_total,
       b.taxable_total - b.charges_taxable - b.cost_total AS gross_profit
  FROM bill b
 WHERE b.status = 'active' AND b.posts_to_ledger;

-- =====================================================================
-- Supabase hardening: link to Supabase Auth, keep schema private, API role
-- =====================================================================
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'auth') THEN
    ALTER TABLE app.app_user
      ADD CONSTRAINT app_user_auth_fk FOREIGN KEY (id) REFERENCES auth.users(id) ON DELETE RESTRICT;
  END IF;
  -- Supabase browser roles must never read or write business tables directly
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'anon') THEN
    REVOKE ALL ON SCHEMA app FROM anon, authenticated;
    REVOKE ALL ON ALL TABLES IN SCHEMA app FROM anon, authenticated;
    REVOKE ALL ON ALL FUNCTIONS IN SCHEMA app FROM anon, authenticated;
  END IF;
  -- Dedicated login role for the Railway API / worker (set password via ALTER ROLE outside source control)
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'billing_api') THEN
    CREATE ROLE billing_api LOGIN NOINHERIT;
  END IF;
END $$;
GRANT USAGE ON SCHEMA app, extensions TO billing_api;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA app TO billing_api;
GRANT USAGE ON ALL SEQUENCES IN SCHEMA app TO billing_api;
GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA app TO billing_api;
REVOKE UPDATE, DELETE ON app.audit_log, app.stock_movement FROM billing_api;   -- append-only for the app
ALTER ROLE billing_api SET search_path = app, extensions;
-- Defence in depth: RLS on with no policies = no access for any non-owner role that bypasses grants
DO $$ DECLARE t text;
BEGIN
  FOR t IN SELECT tablename FROM pg_tables WHERE schemaname = 'app' LOOP
    EXECUTE format('ALTER TABLE app.%I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('CREATE POLICY api_all ON app.%I TO billing_api USING (true) WITH CHECK (true)', t);
  END LOOP;
END $$;

-- =====================================================================
-- Seed data
-- =====================================================================
INSERT INTO location (name, kind, is_default_billing) VALUES
  ('Shop', 'shop', true), ('Godown 1', 'godown', false), ('Godown 2', 'godown', false);

INSERT INTO unit (name, symbol, uqc_code, decimals) VALUES
  ('Numbers', 'nos', 'NOS', 0), ('Bag', 'bag', 'BAG', 0), ('Kilogram', 'kg', 'KGS', 3),
  ('Quintal', 'qtl', 'QTL', 3), ('Tonne', 'ton', 'TON', 3), ('Cubic feet', 'cft', 'CBF', 2),
  ('Brass', 'brass', 'OTH', 2), ('Square feet', 'sqft', 'SQF', 2), ('Litre', 'ltr', 'LTR', 3),
  ('Metre', 'mtr', 'MTR', 2), ('Bundle', 'bdl', 'BDL', 0), ('Load', 'load', 'OTH', 0);

