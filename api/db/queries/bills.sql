-- name: NextDocNo :one
SELECT fn_next_doc_no(@doc_type::doc_kind, @doc_date::date)::text AS doc_no;

-- name: FY :one
SELECT fn_fy(@d::date)::text AS fy;

-- name: InsertBill :one
INSERT INTO bill (bill_type, bill_no, fy, bill_date, status, source_bill_id, party_id, cust_name, cust_gstin,
                  cust_address, cust_mobile, site_address, place_of_supply, is_interstate, vehicle_no, affects_stock,
                  posts_to_ledger, items_taxable, charges_taxable, bill_discount, taxable_total, cgst, sgst, igst,
                  round_off, grand_total, einv_status, notes, created_by)
VALUES (@bill_type, sqlc.narg('bill_no'), sqlc.narg('fy'), @bill_date, @status, sqlc.narg('source_bill_id'),
        sqlc.narg('party_id'), @cust_name, sqlc.narg('cust_gstin'), sqlc.narg('cust_address'), sqlc.narg('cust_mobile'),
        sqlc.narg('site_address'), @place_of_supply, @is_interstate, sqlc.narg('vehicle_no'), @affects_stock,
        @posts_to_ledger, @items_taxable, @charges_taxable, @bill_discount, @taxable_total, @cgst, @sgst, @igst,
        @round_off, @grand_total, @einv_status, sqlc.narg('notes'), @created_by)
RETURNING id;

-- name: UpdateBillContent :exec
UPDATE bill SET bill_date = @bill_date, party_id = sqlc.narg('party_id'), cust_name = @cust_name,
       cust_gstin = sqlc.narg('cust_gstin'), cust_address = sqlc.narg('cust_address'),
       cust_mobile = sqlc.narg('cust_mobile'), site_address = sqlc.narg('site_address'),
       place_of_supply = @place_of_supply, is_interstate = @is_interstate, vehicle_no = sqlc.narg('vehicle_no'),
       items_taxable = @items_taxable, charges_taxable = @charges_taxable, bill_discount = @bill_discount,
       taxable_total = @taxable_total, cgst = @cgst, sgst = @sgst, igst = @igst, round_off = @round_off,
       grand_total = @grand_total, einv_status = @einv_status, notes = sqlc.narg('notes'),
       updated_by = @user_id, updated_at = now()
WHERE id = @id;

-- name: ActivateBill :exec
UPDATE bill SET status = 'active', bill_no = @bill_no, fy = @fy, updated_by = @user_id, updated_at = now()
WHERE id = @id;

-- name: BumpBillVersion :one
UPDATE bill SET version = version + 1, updated_by = @user_id, updated_at = now() WHERE id = @id RETURNING version;

-- name: CancelBillRow :exec
UPDATE bill SET status = 'cancelled', cancel_reason = @reason, cancelled_by = @user_id, cancelled_at = now(),
       version = version + 1, updated_by = @user_id, updated_at = now()
WHERE id = @id;

-- name: SetBillEInvoice :exec
UPDATE bill SET einv_status = @einv_status, einv_error = sqlc.narg('einv_error'), updated_at = now() WHERE id = @id;

-- name: GetBill :one
SELECT b.*, coalesce(u.full_name, '')::text AS created_by_name
FROM bill b LEFT JOIN app_user u ON u.id = b.created_by
WHERE b.id = $1;

-- name: LockBill :one
SELECT * FROM bill WHERE id = $1 FOR UPDATE;

-- name: BillLines :many
SELECT bl.*, l.name AS location_name, u.symbol AS unit_symbol
FROM bill_line bl
JOIN location l ON l.id = bl.location_id
JOIN unit u ON u.id = bl.unit_id
WHERE bl.bill_id = $1
ORDER BY bl.line_no;

-- name: InsertBillLine :one
INSERT INTO bill_line (bill_id, line_no, item_id, location_id, description, hsn_code, qty, unit_id, factor_to_base,
                       rate, discount_pct, discount_amt, taxable_value, gst_rate, cgst, sgst, igst, line_total,
                       unit_cost)
VALUES (@bill_id, @line_no, @item_id, @location_id, @description, @hsn_code, @qty, @unit_id, @factor_to_base,
        @rate, @discount_pct, @discount_amt, @taxable_value, @gst_rate, @cgst, @sgst, @igst, @line_total,
        sqlc.narg('unit_cost'))
RETURNING id;

-- name: DeleteBillLines :exec
DELETE FROM bill_line WHERE bill_id = $1;

-- name: BillCharges :many
SELECT * FROM bill_charge WHERE bill_id = $1 ORDER BY id;

-- name: InsertBillCharge :exec
INSERT INTO bill_charge (bill_id, kind, description, sac_code, amount, gst_rate, cgst, sgst, igst)
VALUES (@bill_id, @kind, @description, sqlc.narg('sac_code'), @amount, @gst_rate, @cgst, @sgst, @igst);

-- name: DeleteBillCharges :exec
DELETE FROM bill_charge WHERE bill_id = $1;

-- name: BillPayments :many
SELECT p.id AS payment_id, p.doc_no, p.mode, a.amount, p.pay_date
FROM payment_allocation a JOIN payment p ON p.id = a.payment_id
WHERE a.ref_type = @ref_type AND a.ref_id = @bill_id AND p.status = 'active'
  AND coalesce(p.cheque_status, 'cleared') <> 'bounced'
ORDER BY p.pay_date, p.id;

-- name: ActiveAccountCopy :one
SELECT id, coalesce(bill_no, '')::text AS bill_no FROM bill
WHERE source_bill_id = $1 AND status <> 'cancelled'
ORDER BY id DESC LIMIT 1;

-- name: PostBill :exec
SELECT fn_post_bill(@bill_id::bigint, @user_id::uuid);

-- name: UnpostBill :exec
SELECT fn_unpost_bill(@bill_id::bigint, @user_id::uuid);

-- name: RefreshBillPaid :one
UPDATE bill b SET amount_paid = coalesce((
    SELECT sum(a.amount) FROM payment_allocation a JOIN payment p ON p.id = a.payment_id
     WHERE a.ref_type = (CASE b.bill_type WHEN 'cash' THEN 'cash_bill' ELSE 'account_bill' END)::doc_kind
       AND a.ref_id = b.id AND p.status = 'active' AND coalesce(p.cheque_status, 'cleared') <> 'bounced'), 0)
WHERE b.id = $1
RETURNING b.amount_paid, b.balance_due;

-- name: InsertBillRevision :exec
INSERT INTO bill_revision (bill_id, version, snapshot, reason, edited_by)
VALUES (@bill_id, @version, @snapshot, @reason, @edited_by);

-- name: BillRevisions :many
SELECT r.version, r.reason, coalesce(u.full_name, '')::text AS edited_by_name, r.edited_at, r.snapshot
FROM bill_revision r LEFT JOIN app_user u ON u.id = r.edited_by
WHERE r.bill_id = $1
ORDER BY r.version DESC;

-- name: BillHasReturns :one
SELECT EXISTS (SELECT 1 FROM credit_note WHERE bill_id = $1 AND status IN ('pending', 'approved'))::bool AS has;

-- name: DeleteDraftBill :execrows
DELETE FROM bill WHERE id = $1 AND status = 'draft';

-- name: ListBills :many
SELECT b.id, b.bill_type, b.bill_no, b.bill_date, b.status, b.cust_name, b.party_id, b.grand_total, b.balance_due,
       b.amount_paid, b.einv_status, b.source_bill_id, b.version, coalesce(ac.id, 0)::bigint AS account_copy_id,
       coalesce(u.full_name, '')::text AS created_by_name, count(*) OVER ()::bigint AS total
FROM bill b
LEFT JOIN app_user u ON u.id = b.created_by
LEFT JOIN LATERAL (SELECT c.id FROM bill c WHERE c.source_bill_id = b.id AND c.status <> 'cancelled'
                   ORDER BY c.id DESC LIMIT 1) ac ON true
WHERE (sqlc.narg('bill_type')::bill_kind IS NULL OR b.bill_type = sqlc.narg('bill_type')::bill_kind)
  AND (sqlc.narg('status')::doc_status IS NULL OR b.status = sqlc.narg('status')::doc_status)
  AND (sqlc.narg('payment_status')::text IS NULL OR
       (CASE WHEN b.balance_due <= 0 THEN 'paid' WHEN b.amount_paid > 0 THEN 'partial' ELSE 'unpaid' END)
         = sqlc.narg('payment_status')::text)
  AND (sqlc.narg('einv_status')::einv_state IS NULL OR b.einv_status = sqlc.narg('einv_status')::einv_state)
  AND (sqlc.narg('party_id')::bigint IS NULL OR b.party_id = sqlc.narg('party_id')::bigint)
  AND (sqlc.narg('location_id')::smallint IS NULL OR EXISTS (
        SELECT 1 FROM bill_line bl WHERE bl.bill_id = b.id AND bl.location_id = sqlc.narg('location_id')::smallint))
  AND (sqlc.narg('from_date')::date IS NULL OR b.bill_date >= sqlc.narg('from_date')::date)
  AND (sqlc.narg('to_date')::date IS NULL OR b.bill_date <= sqlc.narg('to_date')::date)
  AND (@q::text = '' OR b.bill_no ILIKE '%' || @q::text || '%' OR b.cust_name ILIKE '%' || @q::text || '%'
       OR b.cust_mobile ILIKE '%' || @q::text || '%')
  AND (NOT @unlinked::bool OR (b.bill_type = 'account' AND b.source_bill_id IS NULL))
ORDER BY
  CASE WHEN @sort::text = 'bill_date'   AND NOT @descending::bool THEN b.bill_date END ASC,
  CASE WHEN @sort::text = 'bill_date'   AND @descending::bool     THEN b.bill_date END DESC,
  CASE WHEN @sort::text = 'bill_no'     AND NOT @descending::bool THEN b.bill_no END ASC,
  CASE WHEN @sort::text = 'bill_no'     AND @descending::bool     THEN b.bill_no END DESC,
  CASE WHEN @sort::text = 'grand_total' AND NOT @descending::bool THEN b.grand_total END ASC,
  CASE WHEN @sort::text = 'grand_total' AND @descending::bool     THEN b.grand_total END DESC,
  CASE WHEN @sort::text = 'created_at'  AND NOT @descending::bool THEN b.created_at END ASC,
  CASE WHEN @sort::text = 'created_at'  AND @descending::bool     THEN b.created_at END DESC,
  b.id DESC
LIMIT @lim OFFSET @off;

-- name: BillingItems :many
SELECT i.id, i.name, i.hsn_code, i.gst_rate, i.is_active, i.base_unit_id, i.avg_cost
FROM item i
WHERE i.id = ANY(@ids::bigint[]);

-- name: ActiveLocations :many
SELECT id, name FROM location WHERE is_active ORDER BY id;
