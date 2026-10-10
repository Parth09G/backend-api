-- Sales figures use bills that post to the ledger (Account copies are not
-- counted again, BR-04). Sales = grand total; profit = items taxable value
-- less cost of goods at average cost at the time of sale (BR-06).

-- name: DashStock :one
SELECT count(*)::bigint AS items,
       coalesce(sum(round(t.total * i.avg_cost, 2)), 0)::numeric AS value,
       count(*) FILTER (WHERE t.total > 0 AND t.total <= i.reorder_level)::bigint AS low_count,
       count(*) FILTER (WHERE t.total <= 0)::bigint AS out_count
FROM item i
CROSS JOIN LATERAL (SELECT coalesce(sum(sb.qty), 0) AS total FROM stock_balance sb WHERE sb.item_id = i.id) t
WHERE i.is_active;

-- name: DashStockByLocation :many
SELECT l.id AS location_id, l.name, coalesce(sum(round(sb.qty * i.avg_cost, 2)), 0)::numeric AS value
FROM location l
LEFT JOIN stock_balance sb ON sb.location_id = l.id
LEFT JOIN item i ON i.id = sb.item_id
WHERE l.is_active
GROUP BY l.id, l.name
ORDER BY l.id;

-- name: DashSales :one
SELECT coalesce(sum(grand_total) FILTER (WHERE bill_date = @today::date), 0)::numeric AS today,
       coalesce(sum(grand_total) FILTER (WHERE bill_date BETWEEN @from_date::date AND @to_date::date), 0)::numeric AS period,
       coalesce(sum(grand_total) FILTER (WHERE fy = @fy::text), 0)::numeric AS fy_total,
       coalesce(sum(grand_total) FILTER (WHERE bill_type = 'cash' AND bill_date BETWEEN @from_date::date AND @to_date::date), 0)::numeric AS cash_bills,
       coalesce(sum(grand_total) FILTER (WHERE bill_type = 'account' AND bill_date BETWEEN @from_date::date AND @to_date::date), 0)::numeric AS account_bills,
       count(*) FILTER (WHERE bill_date BETWEEN @from_date::date AND @to_date::date)::bigint AS bill_count,
       coalesce(sum(items_taxable - cost_total) FILTER (WHERE bill_date BETWEEN @from_date::date AND @to_date::date), 0)::numeric AS profit,
       coalesce(sum(items_taxable) FILTER (WHERE bill_date BETWEEN @from_date::date AND @to_date::date), 0)::numeric AS items_taxable
FROM bill
WHERE status = 'active' AND posts_to_ledger;

-- name: DashBalances :one
WITH b AS (
  SELECT p.kind, p.opening_balance + coalesce((SELECT sum(l.debit - l.credit) FROM party_ledger l WHERE l.party_id = p.id), 0) AS bal
  FROM party p
)
SELECT coalesce(sum(bal) FILTER (WHERE bal > 0), 0)::numeric AS receivables,
       coalesce(-sum(bal) FILTER (WHERE bal < 0), 0)::numeric AS payables
FROM b;

-- name: DashChequesPending :one
SELECT coalesce(sum(amount), 0)::numeric AS amount
FROM payment WHERE direction = 'received' AND status = 'active' AND cheque_status = 'pending';

-- name: DashCollections :many
SELECT mode::text AS mode, coalesce(sum(amount), 0)::numeric AS amount
FROM payment
WHERE direction = 'received' AND status = 'active' AND coalesce(cheque_status, 'cleared') <> 'bounced'
  AND pay_date BETWEEN @from_date::date AND @to_date::date
GROUP BY mode
ORDER BY mode;

-- name: DashTrend :many
SELECT to_char(date_trunc(@bucket::text, bill_date), CASE WHEN @bucket::text = 'month' THEN 'YYYY-MM' ELSE 'YYYY-MM-DD' END)::text AS bucket,
       coalesce(sum(grand_total), 0)::numeric AS sales,
       coalesce(sum(items_taxable - cost_total), 0)::numeric AS profit,
       count(*)::bigint AS bills
FROM bill
WHERE status = 'active' AND posts_to_ledger
  AND bill_date BETWEEN @from_date::date AND @to_date::date
  AND (sqlc.narg('bill_type')::bill_kind IS NULL OR bill_type = sqlc.narg('bill_type')::bill_kind)
GROUP BY 1
ORDER BY 1;

-- name: DashCategories :many
WITH sold AS (
  SELECT i.category_id, sum(bl.taxable_value) AS sales,
         sum(bl.taxable_value - round(bl.base_qty * coalesce(bl.unit_cost, 0), 2)) AS profit
  FROM bill_line bl
  JOIN bill b ON b.id = bl.bill_id
  JOIN item i ON i.id = bl.item_id
  WHERE b.status = 'active' AND b.posts_to_ledger AND b.bill_date BETWEEN @from_date::date AND @to_date::date
    AND (sqlc.narg('location_id')::smallint IS NULL OR bl.location_id = sqlc.narg('location_id')::smallint)
  GROUP BY i.category_id
), stock AS (
  SELECT i.category_id, count(DISTINCT i.id) FILTER (WHERE sb.qty > 0) AS items,
         sum(round(sb.qty * i.avg_cost, 2)) AS value
  FROM item i
  JOIN stock_balance sb ON sb.item_id = i.id
  WHERE sqlc.narg('location_id')::smallint IS NULL OR sb.location_id = sqlc.narg('location_id')::smallint
  GROUP BY i.category_id
)
SELECT c.id AS category_id, c.name, coalesce(st.items, 0)::bigint AS stock_items,
       coalesce(st.value, 0)::numeric AS stock_value, coalesce(so.sales, 0)::numeric AS sales,
       coalesce(so.profit, 0)::numeric AS profit
FROM category c
LEFT JOIN sold so ON so.category_id = c.id
LEFT JOIN stock st ON st.category_id = c.id
WHERE c.is_active
ORDER BY coalesce(so.sales, 0) DESC, c.name;

-- name: DashTopItems :many
SELECT i.id AS item_id, i.name, u.symbol AS unit,
       coalesce(s.qty, 0)::numeric AS qty_sold, coalesce(s.sales, 0)::numeric AS sales
FROM item i
JOIN unit u ON u.id = i.base_unit_id
LEFT JOIN (
  SELECT bl.item_id, sum(bl.base_qty) AS qty, sum(bl.taxable_value) AS sales
  FROM bill_line bl JOIN bill b ON b.id = bl.bill_id
  WHERE b.status = 'active' AND b.posts_to_ledger AND b.bill_date BETWEEN @from_date::date AND @to_date::date
  GROUP BY bl.item_id
) s ON s.item_id = i.id
WHERE i.is_active
ORDER BY CASE WHEN @slow::bool THEN coalesce(s.qty, 0) END ASC,
         CASE WHEN NOT @slow::bool THEN coalesce(s.qty, 0) END DESC,
         i.name
LIMIT @lim;

-- name: DashLowStockIDs :many
SELECT i.id
FROM item i
CROSS JOIN LATERAL (
  SELECT coalesce(sum(sb.qty), 0) AS qty FROM stock_balance sb
   WHERE sb.item_id = i.id
     AND (sqlc.narg('location_id')::smallint IS NULL OR sb.location_id = sqlc.narg('location_id')::smallint)) t
WHERE i.is_active AND t.qty <= i.reorder_level
ORDER BY t.qty, i.name
LIMIT 500;

-- name: DashPending :one
SELECT
  ((SELECT count(*) FROM bill WHERE status = 'active' AND einv_status = 'failed')
   + (SELECT count(*) FROM credit_note WHERE einv_status = 'failed'))::bigint AS einvoice_failed,
  ((SELECT count(*) FROM bill WHERE status = 'active' AND einv_status = 'pending')
   + (SELECT count(*) FROM credit_note WHERE einv_status = 'pending'))::bigint AS einvoice_pending,
  (SELECT count(*) FROM credit_note WHERE status = 'pending')::bigint AS returns_pending,
  (SELECT count(*) FROM notification WHERE kind = 'stock_short' AND created_at >= @today::date)::bigint AS blocked_bills_today,
  (SELECT count(*) FROM bill b WHERE b.status <> 'draft' AND b.posts_to_ledger
     AND NOT EXISTS (SELECT 1 FROM tally_export_item t
                      WHERE t.ref_type = (CASE b.bill_type WHEN 'cash' THEN 'cash_bill' ELSE 'account_bill' END)::doc_kind
                        AND t.ref_id = b.id AND t.doc_version = b.version))::bigint AS tally_unexported,
  (SELECT count(*) FROM payment WHERE direction = 'received' AND status = 'active' AND cheque_status = 'pending'
     AND coalesce(cheque_date, pay_date) <= @today::date)::bigint AS cheques_due;
