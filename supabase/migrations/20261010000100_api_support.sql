-- =====================================================================
-- API support (openapi.yaml v1.0 / Billing_API_Design_v1). Applied after
-- schema.sql. Everything the REST API needs that schema.sql does not have.
-- =====================================================================

SET search_path = app, extensions;

-- ---------------------------------------------------------------------
-- River job queue (Go). River's own migrator creates its tables here.
-- ---------------------------------------------------------------------
CREATE SCHEMA IF NOT EXISTS river;
REVOKE ALL ON SCHEMA river FROM PUBLIC;
GRANT USAGE, CREATE ON SCHEMA river TO billing_api;

-- ---------------------------------------------------------------------
-- Settings exposed by /settings, /settings/einvoice-credentials and
-- /settings/tally-mapping
-- ---------------------------------------------------------------------
ALTER TABLE shop_settings
  ADD COLUMN number_prefixes       jsonb NOT NULL DEFAULT '{}'::jsonb,  -- {"cash_bill":"C", ...}; ≤ 4 chars each
  ADD COLUMN notification_channels jsonb NOT NULL DEFAULT '{}'::jsonb,
  ADD COLUMN tally_mapping         jsonb NOT NULL DEFAULT '{}'::jsonb,
  ADD COLUMN einv_provider         text,
  ADD COLUMN einv_environment      text CHECK (einv_environment IN ('sandbox', 'production')),
  ADD COLUMN einv_username         text,
  ADD COLUMN einv_client_id        text,
  ADD COLUMN einv_secret           bytea,   -- AES-256-GCM of {password, client_secret}; key only in Railway env
  ADD COLUMN locked_until          date;    -- FR-ED-04: bills dated on/before this cannot be edited

-- Document numbers honour settings.number_prefixes (when a series starts).
CREATE OR REPLACE FUNCTION fn_next_doc_no(p_type doc_kind, p_date date) RETURNS text LANGUAGE plpgsql
  SET search_path = app, extensions AS $$
DECLARE v_fy char(5) := fn_fy(p_date); v_base text; v_prefix text; v_no int;
BEGIN
  SELECT nullif(number_prefixes ->> p_type::text, '') INTO v_base FROM shop_settings WHERE id = 1;
  v_base := coalesce(v_base,
            CASE p_type WHEN 'cash_bill' THEN 'C' WHEN 'account_bill' THEN 'A' WHEN 'credit_note' THEN 'CN'
                        WHEN 'receipt' THEN 'R' WHEN 'payment_voucher' THEN 'PV' WHEN 'purchase' THEN 'P'
                        WHEN 'purchase_return' THEN 'PR' WHEN 'transfer' THEN 'T' ELSE 'ADJ' END);
  INSERT INTO doc_series (doc_type, fy, prefix)
  VALUES (p_type, v_fy, v_base || '/' || v_fy || '/')
  ON CONFLICT DO NOTHING;
  UPDATE doc_series SET next_no = next_no + 1
   WHERE doc_type = p_type AND fy = v_fy
  RETURNING prefix, next_no - 1 INTO v_prefix, v_no;
  RETURN v_prefix || lpad(v_no::text, 4, '0');
END $$;

-- Reverse the average-cost effect of goods leaving at their purchase cost
-- (purchase cancel / purchase return). Call after fn_stock_out.
CREATE FUNCTION fn_remove_cost(p_item bigint, p_qty numeric, p_cost numeric) RETURNS void LANGUAGE plpgsql
  SET search_path = app, extensions AS $$
DECLARE v_total numeric; v_avg numeric;
BEGIN
  SELECT avg_cost INTO v_avg FROM item WHERE id = p_item FOR UPDATE;
  SELECT coalesce(sum(qty), 0) INTO v_total FROM stock_balance WHERE item_id = p_item;   -- already reduced
  IF v_total > 0 AND p_cost IS NOT NULL THEN
    UPDATE item SET avg_cost = greatest(round(((v_total + p_qty) * v_avg - p_qty * p_cost) / v_total, 4), 0),
                    updated_at = now()
     WHERE id = p_item;
  END IF;
END $$;

-- ---------------------------------------------------------------------
-- Users: login tracking, and at least one active Super Admin (FR-AU-02)
-- ---------------------------------------------------------------------
ALTER TABLE app_user ADD COLUMN last_session_id uuid;

CREATE FUNCTION tg_keep_super_admin() RETURNS trigger LANGUAGE plpgsql
  SET search_path = app, extensions AS $$
BEGIN
  IF EXISTS (SELECT 1 FROM app_user)
     AND NOT EXISTS (SELECT 1 FROM app_user WHERE role = 'super_admin' AND is_active) THEN
    RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'LAST_SUPER_ADMIN',
      DETAIL = 'At least one active Super Admin is required';
  END IF;
  RETURN NULL;
END $$;
CREATE TRIGGER app_user_keep_super_admin AFTER UPDATE OR DELETE ON app_user
  FOR EACH STATEMENT EXECUTE FUNCTION tg_keep_super_admin();

-- ---------------------------------------------------------------------
-- Bills, payments, returns, purchases: fields the API needs
-- ---------------------------------------------------------------------
ALTER TABLE bill
  ADD COLUMN einv_error text;                      -- last e-invoice / e-way bill failure message

ALTER TABLE payment
  ADD COLUMN bill_id       bigint REFERENCES bill(id),   -- set when taken on the bill screen
  ADD COLUMN cleared_date  date,
  ADD COLUMN bounced_date  date,
  ADD COLUMN cancel_reason text,
  ADD COLUMN cancelled_by  uuid REFERENCES app_user(id),
  ADD COLUMN cancelled_at  timestamptz;

ALTER TABLE credit_note
  ADD COLUMN refund            jsonb,                          -- refund split entered with the return
  ADD COLUMN refund_payment_id bigint REFERENCES payment(id),
  ADD COLUMN reject_reason     text,
  ADD COLUMN einv_error        text;

ALTER TABLE purchase
  ADD COLUMN cancel_reason text,
  ADD COLUMN cancelled_by  uuid REFERENCES app_user(id),
  ADD COLUMN cancelled_at  timestamptz;

-- Document number for a (ref_type, ref_id) pair: ledgers and stock history.
CREATE FUNCTION fn_ref_no(p_type doc_kind, p_id bigint) RETURNS text LANGUAGE sql STABLE
  SET search_path = app, extensions AS $$
  SELECT CASE p_type
    WHEN 'cash_bill'       THEN (SELECT bill_no FROM bill WHERE id = p_id)
    WHEN 'account_bill'    THEN (SELECT bill_no FROM bill WHERE id = p_id)
    WHEN 'credit_note'     THEN (SELECT cn_no FROM credit_note WHERE id = p_id)
    WHEN 'receipt'         THEN (SELECT doc_no FROM payment WHERE id = p_id)
    WHEN 'payment_voucher' THEN (SELECT doc_no FROM payment WHERE id = p_id)
    WHEN 'purchase'        THEN (SELECT doc_no FROM purchase WHERE id = p_id)
    WHEN 'purchase_return' THEN (SELECT doc_no FROM purchase_return WHERE id = p_id)
    WHEN 'transfer'        THEN (SELECT doc_no FROM stock_transfer WHERE id = p_id)
    WHEN 'adjustment'      THEN (SELECT doc_no FROM stock_adjustment WHERE id = p_id)
  END
$$;

-- ---------------------------------------------------------------------
-- Background jobs shown by GET /jobs/{id} (imports, exports, big reports)
-- ---------------------------------------------------------------------
CREATE TABLE job (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  kind         text NOT NULL CHECK (kind IN ('item_import', 'tally_import', 'tally_export', 'report_export', 'einvoice')),
  status       text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'succeeded', 'failed')),
  progress     int  NOT NULL DEFAULT 0 CHECK (progress BETWEEN 0 AND 100),
  params       jsonb NOT NULL DEFAULT '{}'::jsonb,
  result       jsonb,
  result_path  text,                                -- file in the private Storage bucket
  error        text,
  created_by   uuid REFERENCES app_user(id),
  created_at   timestamptz NOT NULL DEFAULT now(),
  finished_at  timestamptz
);

-- Tally import previews: parsed file kept until committed (FR-TI-01)
CREATE TABLE tally_import (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  source        text NOT NULL CHECK (source IN ('tally_xml', 'excel')),
  cutover_date  date NOT NULL,
  scope         text[] NOT NULL,
  data          jsonb NOT NULL,
  counts        jsonb NOT NULL DEFAULT '{}'::jsonb,
  issues        jsonb NOT NULL DEFAULT '[]'::jsonb,
  status        text NOT NULL DEFAULT 'preview' CHECK (status IN ('preview', 'committing', 'committed', 'failed')),
  job_id        uuid REFERENCES job(id),
  expires_at    timestamptz NOT NULL,
  created_by    uuid REFERENCES app_user(id),
  created_at    timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------
-- Indexes for list filters, ledgers and search (NFR-01)
-- ---------------------------------------------------------------------
CREATE INDEX payment_party_idx        ON payment (party_id, pay_date);
CREATE INDEX payment_bill_idx         ON payment (bill_id);
CREATE INDEX payment_alloc_ref_idx    ON payment_allocation (ref_type, ref_id);
CREATE INDEX credit_note_bill_idx     ON credit_note (bill_id);
CREATE INDEX credit_note_line_bl_idx  ON credit_note_line (bill_line_id);
CREATE INDEX stock_movement_loc_idx   ON stock_movement (item_id, location_id, id);
CREATE INDEX purchase_supplier_idx    ON purchase (supplier_id, received_date);
CREATE INDEX bill_cust_name_trgm      ON bill USING gin (cust_name extensions.gin_trgm_ops);
CREATE INDEX bill_no_trgm             ON bill USING gin (bill_no extensions.gin_trgm_ops);
CREATE INDEX notification_user_idx    ON notification (user_id, for_role, is_read);

-- ---------------------------------------------------------------------
-- Privileges: history stays append-only; new objects for billing_api only
-- ---------------------------------------------------------------------
REVOKE UPDATE, DELETE, TRUNCATE ON party_ledger, bill_revision, einvoice_log FROM billing_api;

GRANT SELECT, INSERT, UPDATE, DELETE ON job, tally_import TO billing_api;
GRANT USAGE ON ALL SEQUENCES IN SCHEMA app TO billing_api;
GRANT EXECUTE ON FUNCTION fn_next_doc_no(doc_kind, date), fn_remove_cost(bigint, numeric, numeric) TO billing_api;

DO $$
DECLARE t text;
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'anon') THEN
    REVOKE ALL ON SCHEMA river FROM anon, authenticated;
    REVOKE ALL ON ALL TABLES    IN SCHEMA app FROM anon, authenticated;
    REVOKE ALL ON ALL FUNCTIONS IN SCHEMA app FROM anon, authenticated;
  END IF;
  FOR t IN SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
            WHERE n.nspname = 'app' AND c.relkind = 'r' AND NOT c.relrowsecurity LOOP
    EXECUTE format('ALTER TABLE app.%I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('CREATE POLICY api_all ON app.%I TO billing_api USING (true) WITH CHECK (true)', t);
  END LOOP;
END $$;

REVOKE EXECUTE ON ALL FUNCTIONS IN SCHEMA app FROM PUBLIC;
GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA app TO billing_api;
