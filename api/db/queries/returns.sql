-- name: InsertCreditNote :one
INSERT INTO credit_note (cn_date, bill_id, party_id, status, reason, taxable_total, cgst, sgst, igst, grand_total,
                         settlement, refund, created_by)
VALUES (@cn_date, @bill_id, sqlc.narg('party_id'), 'pending', @reason, @taxable_total, @cgst, @sgst, @igst,
        @grand_total, @settlement, sqlc.narg('refund'), @created_by)
RETURNING id;

-- name: InsertCreditNoteLine :exec
INSERT INTO credit_note_line (credit_note_id, bill_line_id, item_id, location_id, qty, unit_id, factor_to_base, rate,
                              taxable_value, gst_rate, cgst, sgst, igst, restock)
VALUES (@credit_note_id, @bill_line_id, @item_id, @location_id, @qty, @unit_id, @factor_to_base, @rate,
        @taxable_value, @gst_rate, @cgst, @sgst, @igst, @restock);

-- name: GetCreditNote :one
SELECT cn.*, coalesce(b.bill_no, '')::text AS bill_no, b.cust_name, b.bill_type, b.cust_gstin,
       coalesce(cu.full_name, '')::text AS created_by_name, au.full_name AS approved_by_name
FROM credit_note cn
JOIN bill b ON b.id = cn.bill_id
LEFT JOIN app_user cu ON cu.id = cn.created_by
LEFT JOIN app_user au ON au.id = cn.approved_by
WHERE cn.id = $1;

-- name: LockCreditNote :one
SELECT * FROM credit_note WHERE id = $1 FOR UPDATE;

-- name: CreditNoteLines :many
SELECT cl.*, bl.description, bl.unit_cost AS sale_unit_cost, u.symbol AS unit_symbol
FROM credit_note_line cl
JOIN bill_line bl ON bl.id = cl.bill_line_id
JOIN unit u ON u.id = cl.unit_id
WHERE cl.credit_note_id = $1
ORDER BY cl.id;

-- name: ListCreditNoteIDs :many
SELECT cn.id, count(*) OVER ()::bigint AS total
FROM credit_note cn
WHERE (sqlc.narg('status')::return_status IS NULL OR cn.status = sqlc.narg('status')::return_status)
  AND (sqlc.narg('party_id')::bigint IS NULL OR cn.party_id = sqlc.narg('party_id')::bigint)
  AND (sqlc.narg('from_date')::date IS NULL OR cn.cn_date >= sqlc.narg('from_date')::date)
  AND (sqlc.narg('to_date')::date IS NULL OR cn.cn_date <= sqlc.narg('to_date')::date)
ORDER BY cn.cn_date DESC, cn.id DESC
LIMIT @lim OFFSET @off;

-- name: ApproveCreditNoteRow :exec
UPDATE credit_note SET status = 'approved', cn_no = @cn_no, fy = @fy, approved_by = @user_id, approved_at = now(),
       einv_status = @einv_status, refund_payment_id = sqlc.narg('refund_payment_id')
WHERE id = @id;

-- name: RejectCreditNoteRow :exec
UPDATE credit_note SET status = 'rejected', reject_reason = @reason, approved_by = @user_id, approved_at = now()
WHERE id = @id;

-- name: ReturnedQty :many
SELECT cl.bill_line_id, sum(cl.base_qty)::numeric AS qty
FROM credit_note_line cl JOIN credit_note c ON c.id = cl.credit_note_id
WHERE cl.bill_line_id = ANY(@ids::bigint[]) AND c.status IN ('pending', 'approved')
GROUP BY cl.bill_line_id;

-- name: SetCreditNoteEInvoice :exec
UPDATE credit_note SET einv_status = @einv_status, einv_error = sqlc.narg('einv_error') WHERE id = @id;
