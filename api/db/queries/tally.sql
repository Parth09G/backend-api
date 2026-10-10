-- name: ListTallySyncs :many
SELECT t.*, coalesce(u.full_name, '')::text AS created_by_name, count(*) OVER ()::bigint AS total
FROM tally_sync t LEFT JOIN app_user u ON u.id = t.created_by
ORDER BY t.created_at DESC, t.id DESC
LIMIT @lim OFFSET @off;

-- name: InsertTallySync :one
INSERT INTO tally_sync (direction, scope, period_from, period_to, file_path, record_count, summary, status, created_by)
VALUES (@direction, @scope, sqlc.narg('period_from'), sqlc.narg('period_to'), sqlc.narg('file_path'), @record_count,
        @summary, @status, sqlc.narg('created_by'))
RETURNING id;

-- name: InsertTallyExportItem :exec
INSERT INTO tally_export_item (sync_id, ref_type, ref_id, doc_version) VALUES (@sync_id, @ref_type, @ref_id, @doc_version)
ON CONFLICT (ref_type, ref_id, doc_version) DO NOTHING;

-- name: TallyBills :many
-- Bills to export: not yet exported in their current version (FR-TI-03).
SELECT b.id, b.bill_type, b.bill_no, b.bill_date, b.status, b.version, b.party_id, b.cust_name,
       coalesce(p.tally_ledger, p.name, '')::text AS party_ledger,
       b.items_taxable, b.cgst, b.sgst, b.igst, b.round_off, b.grand_total,
       EXISTS (SELECT 1 FROM tally_export_item t
                WHERE t.ref_type = (CASE b.bill_type WHEN 'cash' THEN 'cash_bill' ELSE 'account_bill' END)::doc_kind
                  AND t.ref_id = b.id)::bool AS exported_before
FROM bill b LEFT JOIN party p ON p.id = b.party_id
WHERE b.status <> 'draft' AND b.posts_to_ledger AND b.bill_date BETWEEN @from_date::date AND @to_date::date
  AND (@include_all::bool OR NOT EXISTS (
        SELECT 1 FROM tally_export_item t
         WHERE t.ref_type = (CASE b.bill_type WHEN 'cash' THEN 'cash_bill' ELSE 'account_bill' END)::doc_kind
           AND t.ref_id = b.id AND t.doc_version = b.version))
ORDER BY b.bill_date, b.id;

-- name: TallyBillCharges :many
SELECT bill_id, kind, sum(amount)::numeric AS amount
FROM bill_charge WHERE bill_id = ANY(@ids::bigint[])
GROUP BY bill_id, kind;

-- name: TallyCreditNotes :many
SELECT cn.id, cn.cn_no, cn.cn_date, b.bill_type, coalesce(p.tally_ledger, p.name, b.cust_name, '')::text AS party_ledger,
       b.party_id, cn.taxable_total, cn.cgst, cn.sgst, cn.igst, cn.grand_total
FROM credit_note cn JOIN bill b ON b.id = cn.bill_id LEFT JOIN party p ON p.id = b.party_id
WHERE cn.status = 'approved' AND cn.cn_date BETWEEN @from_date::date AND @to_date::date
  AND (@include_all::bool OR NOT EXISTS (
        SELECT 1 FROM tally_export_item t WHERE t.ref_type = 'credit_note' AND t.ref_id = cn.id AND t.doc_version = 1))
ORDER BY cn.cn_date, cn.id;

-- name: TallyPayments :many
SELECT py.id, py.direction, py.doc_no, py.pay_date, py.mode, py.amount, py.status, py.party_id,
       coalesce(p.tally_ledger, p.name, '')::text AS party_ledger, coalesce(py.reference, py.cheque_no, '')::text AS reference,
       (CASE WHEN py.status = 'cancelled' OR py.cheque_status = 'bounced' THEN 2 ELSE 1 END)::int AS version,
       EXISTS (SELECT 1 FROM tally_export_item t
                WHERE t.ref_type = (CASE py.direction WHEN 'received' THEN 'receipt' ELSE 'payment_voucher' END)::doc_kind
                  AND t.ref_id = py.id)::bool AS exported_before
FROM payment py LEFT JOIN party p ON p.id = py.party_id
WHERE py.pay_date BETWEEN @from_date::date AND @to_date::date
  AND (sqlc.narg('direction')::pay_direction IS NULL OR py.direction = sqlc.narg('direction')::pay_direction)
  AND (@include_all::bool OR NOT EXISTS (
        SELECT 1 FROM tally_export_item t
         WHERE t.ref_type = (CASE py.direction WHEN 'received' THEN 'receipt' ELSE 'payment_voucher' END)::doc_kind
           AND t.ref_id = py.id
           AND t.doc_version = (CASE WHEN py.status = 'cancelled' OR py.cheque_status = 'bounced' THEN 2 ELSE 1 END)))
ORDER BY py.pay_date, py.id;

-- name: TallyPurchases :many
SELECT pu.id, pu.doc_no, pu.supplier_invoice_no, pu.supplier_invoice_dt, pu.received_date, pu.status,
       coalesce(p.tally_ledger, p.name)::text AS party_ledger, pu.taxable_total, pu.freight, pu.other_charges,
       pu.cgst, pu.sgst, pu.igst, pu.round_off, pu.grand_total,
       (CASE WHEN pu.status = 'cancelled' THEN 2 ELSE 1 END)::int AS version,
       EXISTS (SELECT 1 FROM tally_export_item t WHERE t.ref_type = 'purchase' AND t.ref_id = pu.id)::bool AS exported_before
FROM purchase pu JOIN party p ON p.id = pu.supplier_id
WHERE pu.received_date BETWEEN @from_date::date AND @to_date::date
  AND (@include_all::bool OR NOT EXISTS (
        SELECT 1 FROM tally_export_item t WHERE t.ref_type = 'purchase' AND t.ref_id = pu.id
           AND t.doc_version = (CASE WHEN pu.status = 'cancelled' THEN 2 ELSE 1 END)))
ORDER BY pu.received_date, pu.id;

-- name: TallyParties :many
SELECT id, kind, coalesce(tally_ledger, name)::text AS ledger, gstin, state_code, billing_address, mobile, opening_balance
FROM party WHERE is_active ORDER BY name;

-- name: TallyItems :many
SELECT i.id, coalesce(i.tally_name, i.name)::text AS name, coalesce(c.tally_group, c.name)::text AS grp,
       u.symbol AS unit, i.hsn_code, i.gst_rate
FROM item i JOIN category c ON c.id = i.category_id JOIN unit u ON u.id = i.base_unit_id
WHERE i.is_active ORDER BY i.name;

-- name: InsertTallyImport :one
INSERT INTO tally_import (source, cutover_date, scope, data, counts, issues, expires_at, created_by)
VALUES (@source, @cutover_date, @scope, @data, @counts, @issues, @expires_at, sqlc.narg('created_by'))
RETURNING id;

-- name: LockTallyImport :one
SELECT * FROM tally_import WHERE id = $1 FOR UPDATE;

-- name: SetTallyImportStatus :exec
UPDATE tally_import SET status = @status, job_id = coalesce(sqlc.narg('job_id'), job_id) WHERE id = @id;

-- name: CategoryByName :one
SELECT id FROM category WHERE lower(name) = lower(@name::text) LIMIT 1;

-- name: UnitByName :one
SELECT id FROM unit WHERE lower(symbol) = lower(@name::text) OR lower(name) = lower(@name::text) LIMIT 1;

-- name: ItemIDByName :one
SELECT id FROM item WHERE lower(name) = lower(@name::text) OR lower(tally_name) = lower(@name::text) LIMIT 1;

-- name: PartyIDByName :one
SELECT id FROM party WHERE lower(name) = lower(@name::text) OR lower(tally_ledger) = lower(@name::text) LIMIT 1;

-- name: LocationByName :one
SELECT id FROM location WHERE lower(name) = lower(@name::text) LIMIT 1;

-- name: InsertImportedCategory :one
INSERT INTO category (name, tally_group, default_gst) VALUES (@name, @name, 18) RETURNING id;

-- name: InsertImportedItem :one
INSERT INTO item (category_id, name, attributes, hsn_code, gst_rate, base_unit_id, tally_name, created_by)
VALUES (@category_id, @name, '{}'::jsonb, @hsn_code, @gst_rate, @base_unit_id, @name, sqlc.narg('created_by'))
RETURNING id;

-- name: InsertImportedParty :one
INSERT INTO party (kind, name, mobile, gstin, state_code, billing_address, opening_balance, opening_date, tally_ledger)
VALUES (@kind, @name, sqlc.narg('mobile'), sqlc.narg('gstin'), sqlc.narg('state_code'), sqlc.narg('billing_address'),
        @opening_balance, @opening_date, @name)
RETURNING id;
