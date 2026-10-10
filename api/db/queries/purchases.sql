-- name: InsertPurchase :one
INSERT INTO purchase (doc_no, fy, supplier_id, supplier_invoice_no, supplier_invoice_dt, received_date, location_id,
                      is_interstate, taxable_total, freight, other_charges, cgst, sgst, igst, round_off, grand_total,
                      notes, created_by)
VALUES (@doc_no, @fy, @supplier_id, @supplier_invoice_no, @supplier_invoice_dt, @received_date, @location_id,
        @is_interstate, @taxable_total, @freight, @other_charges, @cgst, @sgst, @igst, @round_off, @grand_total,
        sqlc.narg('notes'), @created_by)
RETURNING id;

-- name: InsertPurchaseLine :one
INSERT INTO purchase_line (purchase_id, item_id, location_id, qty, unit_id, factor_to_base, rate, discount_amt,
                           taxable_value, gst_rate, cgst, sgst, igst, landed_unit_cost)
VALUES (@purchase_id, @item_id, @location_id, @qty, @unit_id, @factor_to_base, @rate, @discount_amt,
        @taxable_value, @gst_rate, @cgst, @sgst, @igst, @landed_unit_cost)
RETURNING id;

-- name: GetPurchase :one
SELECT pu.*, pt.name AS supplier_name, l.name AS location_name
FROM purchase pu
JOIN party pt ON pt.id = pu.supplier_id
JOIN location l ON l.id = pu.location_id
WHERE pu.id = $1;

-- name: LockPurchase :one
SELECT * FROM purchase WHERE id = $1 FOR UPDATE;

-- name: PurchaseLines :many
SELECT pl.*, i.name AS item_name, u.symbol AS unit_symbol
FROM purchase_line pl
JOIN item i ON i.id = pl.item_id
JOIN unit u ON u.id = pl.unit_id
WHERE pl.purchase_id = $1
ORDER BY pl.id;

-- name: ListPurchaseIDs :many
SELECT pu.id, count(*) OVER ()::bigint AS total
FROM purchase pu
JOIN party pt ON pt.id = pu.supplier_id
WHERE (sqlc.narg('supplier_id')::bigint IS NULL OR pu.supplier_id = sqlc.narg('supplier_id')::bigint)
  AND (sqlc.narg('location_id')::smallint IS NULL OR pu.location_id = sqlc.narg('location_id')::smallint
       OR EXISTS (SELECT 1 FROM purchase_line pl WHERE pl.purchase_id = pu.id
                   AND pl.location_id = sqlc.narg('location_id')::smallint))
  AND (sqlc.narg('from_date')::date IS NULL OR pu.received_date >= sqlc.narg('from_date')::date)
  AND (sqlc.narg('to_date')::date IS NULL OR pu.received_date <= sqlc.narg('to_date')::date)
  AND (@q::text = '' OR pu.doc_no ILIKE '%' || @q::text || '%'
       OR pu.supplier_invoice_no ILIKE '%' || @q::text || '%' OR pt.name ILIKE '%' || @q::text || '%')
ORDER BY
  CASE WHEN @sort::text = 'received_date' AND NOT @descending::bool THEN pu.received_date END ASC,
  CASE WHEN @sort::text = 'received_date' AND @descending::bool     THEN pu.received_date END DESC,
  CASE WHEN @sort::text = 'doc_no'        AND NOT @descending::bool THEN pu.doc_no END ASC,
  CASE WHEN @sort::text = 'doc_no'        AND @descending::bool     THEN pu.doc_no END DESC,
  CASE WHEN @sort::text = 'grand_total'   AND NOT @descending::bool THEN pu.grand_total END ASC,
  CASE WHEN @sort::text = 'grand_total'   AND @descending::bool     THEN pu.grand_total END DESC,
  pu.id DESC
LIMIT @lim OFFSET @off;

-- name: CancelPurchaseRow :exec
UPDATE purchase SET status = 'cancelled', cancel_reason = @reason, cancelled_by = @user_id, cancelled_at = now()
WHERE id = @id;

-- name: InsertPurchaseReturn :one
INSERT INTO purchase_return (doc_no, fy, purchase_id, return_date, reason, grand_total, created_by)
VALUES (@doc_no, @fy, @purchase_id, @return_date, @reason, @grand_total, @created_by)
RETURNING id;

-- name: InsertPurchaseReturnLine :exec
INSERT INTO purchase_return_line (purchase_return_id, purchase_line_id, item_id, location_id, base_qty, rate, amount)
VALUES (@purchase_return_id, @purchase_line_id, @item_id, @location_id, @base_qty, @rate, @amount);

-- name: PurchaseReturnedQty :many
SELECT prl.purchase_line_id, sum(prl.base_qty)::numeric AS qty
FROM purchase_return_line prl
WHERE prl.purchase_line_id = ANY(@ids::bigint[])
GROUP BY prl.purchase_line_id;

-- name: PurchaseHasReturns :one
SELECT EXISTS (SELECT 1 FROM purchase_return WHERE purchase_id = $1)::bool AS has;

-- name: DetachPurchaseAllocations :exec
-- On purchase cancel the dealer payments stay as advances (on account).
DELETE FROM payment_allocation WHERE ref_type = 'purchase' AND ref_id = $1;

-- name: SupplierState :one
SELECT coalesce(state_code, '')::text AS state_code FROM party WHERE id = $1;
