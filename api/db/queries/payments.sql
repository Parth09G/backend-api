-- name: InsertPayment :one
INSERT INTO payment (direction, doc_no, fy, party_id, pay_date, mode, amount, reference, cheque_no, cheque_date,
                     bank_name, cheque_status, notes, bill_id, created_by)
VALUES (@direction, @doc_no, @fy, sqlc.narg('party_id'), @pay_date, @mode, @amount, sqlc.narg('reference'),
        sqlc.narg('cheque_no'), sqlc.narg('cheque_date'), sqlc.narg('bank_name'), sqlc.narg('cheque_status'),
        sqlc.narg('notes'), sqlc.narg('bill_id'), @created_by)
RETURNING id;

-- name: InsertAllocation :exec
INSERT INTO payment_allocation (payment_id, ref_type, ref_id, amount) VALUES (@payment_id, @ref_type, @ref_id, @amount);

-- name: GetPayment :one
SELECT p.*, coalesce(pt.name, b.cust_name, '')::text AS party_name
FROM payment p
LEFT JOIN party pt ON pt.id = p.party_id
LEFT JOIN bill b ON b.id = p.bill_id
WHERE p.id = $1;

-- name: LockPayment :one
SELECT * FROM payment WHERE id = $1 FOR UPDATE;

-- name: PaymentAllocations :many
SELECT a.payment_id, a.ref_type, a.ref_id, coalesce(fn_ref_no(a.ref_type, a.ref_id), '')::text AS ref_no, a.amount
FROM payment_allocation a
WHERE a.payment_id = ANY(@ids::bigint[])
ORDER BY a.payment_id, a.ref_id;

-- name: ListPaymentIDs :many
SELECT p.id, count(*) OVER ()::bigint AS total
FROM payment p
WHERE (sqlc.narg('direction')::pay_direction IS NULL OR p.direction = sqlc.narg('direction')::pay_direction)
  AND (sqlc.narg('mode')::pay_mode IS NULL OR p.mode = sqlc.narg('mode')::pay_mode)
  AND (sqlc.narg('cheque_status')::cheque_state IS NULL OR p.cheque_status = sqlc.narg('cheque_status')::cheque_state)
  AND (sqlc.narg('party_id')::bigint IS NULL OR p.party_id = sqlc.narg('party_id')::bigint)
  AND (sqlc.narg('from_date')::date IS NULL OR p.pay_date >= sqlc.narg('from_date')::date)
  AND (sqlc.narg('to_date')::date IS NULL OR p.pay_date <= sqlc.narg('to_date')::date)
ORDER BY p.pay_date DESC, p.id DESC
LIMIT @lim OFFSET @off;

-- name: PaymentsByIDs :many
SELECT p.*, coalesce(pt.name, b.cust_name, '')::text AS party_name
FROM payment p
LEFT JOIN party pt ON pt.id = p.party_id
LEFT JOIN bill b ON b.id = p.bill_id
WHERE p.id = ANY(@ids::bigint[]);

-- name: SetChequeCleared :exec
UPDATE payment SET cheque_status = 'cleared', cleared_date = @cleared_date WHERE id = @id;

-- name: SetChequeBounced :exec
UPDATE payment SET cheque_status = 'bounced', bounced_date = @bounced_date,
       notes = coalesce(sqlc.narg('notes'), notes)
WHERE id = @id;

-- name: CancelPaymentRow :exec
UPDATE payment SET status = 'cancelled', cancel_reason = @reason, cancelled_by = @user_id, cancelled_at = now()
WHERE id = @id;

-- name: OpenBillsForParty :many
SELECT b.id, b.bill_type, b.bill_date, b.balance_due
FROM bill b
WHERE b.party_id = $1 AND b.status = 'active' AND b.posts_to_ledger AND b.balance_due > 0
ORDER BY b.bill_date, b.id
FOR UPDATE;

-- name: OpenPurchasesForParty :many
SELECT pu.id, pu.received_date, (pu.grand_total - coalesce(paid.amt, 0))::numeric AS due
FROM purchase pu
LEFT JOIN LATERAL (
  SELECT sum(a.amount) AS amt FROM payment_allocation a JOIN payment p ON p.id = a.payment_id
   WHERE a.ref_type = 'purchase' AND a.ref_id = pu.id AND p.status = 'active'
     AND coalesce(p.cheque_status, 'cleared') <> 'bounced') paid ON true
WHERE pu.supplier_id = $1 AND pu.status = 'active' AND pu.grand_total - coalesce(paid.amt, 0) > 0
ORDER BY pu.received_date, pu.id;

-- name: PurchasePaid :one
SELECT coalesce(sum(a.amount), 0)::numeric AS paid
FROM payment_allocation a JOIN payment p ON p.id = a.payment_id
WHERE a.ref_type = 'purchase' AND a.ref_id = $1 AND p.status = 'active'
  AND coalesce(p.cheque_status, 'cleared') <> 'bounced';

-- name: AllocatedBills :many
SELECT DISTINCT a.ref_id FROM payment_allocation a
WHERE a.payment_id = $1 AND a.ref_type IN ('cash_bill', 'account_bill');

-- name: DetachBillAllocations :exec
-- On bill cancel: receipts recorded later (not on the bill screen) go back on account.
DELETE FROM payment_allocation a
USING payment p
WHERE p.id = a.payment_id AND a.ref_type = @ref_type AND a.ref_id = @bill_id
  AND p.bill_id IS DISTINCT FROM @bill_id::bigint;

-- name: BillScreenPayments :many
SELECT * FROM payment WHERE bill_id = $1 AND status = 'active' ORDER BY id;
