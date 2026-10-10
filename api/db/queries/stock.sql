-- name: InsertTransfer :one
INSERT INTO stock_transfer (doc_no, fy, transfer_date, from_location_id, to_location_id, vehicle_no, notes, created_by)
VALUES (@doc_no, @fy, @transfer_date, @from_location_id, @to_location_id, sqlc.narg('vehicle_no'),
        sqlc.narg('notes'), @created_by)
RETURNING id;

-- name: InsertTransferLine :one
INSERT INTO stock_transfer_line (transfer_id, item_id, base_qty) VALUES (@transfer_id, @item_id, @base_qty)
RETURNING id;

-- name: GetTransfer :one
SELECT t.*, coalesce(u.full_name, '')::text AS created_by_name
FROM stock_transfer t LEFT JOIN app_user u ON u.id = t.created_by
WHERE t.id = $1;

-- name: TransferLines :many
SELECT tl.transfer_id, tl.item_id, i.name AS item_name, tl.base_qty, u.symbol AS unit_symbol
FROM stock_transfer_line tl
JOIN item i ON i.id = tl.item_id
JOIN unit u ON u.id = i.base_unit_id
WHERE tl.transfer_id = ANY(@ids::bigint[])
ORDER BY tl.transfer_id, tl.id;

-- name: ListTransfers :many
SELECT t.*, coalesce(u.full_name, '')::text AS created_by_name, count(*) OVER ()::bigint AS total
FROM stock_transfer t LEFT JOIN app_user u ON u.id = t.created_by
WHERE (sqlc.narg('location_id')::smallint IS NULL OR t.from_location_id = sqlc.narg('location_id')::smallint
       OR t.to_location_id = sqlc.narg('location_id')::smallint)
  AND (sqlc.narg('from_date')::date IS NULL OR t.transfer_date >= sqlc.narg('from_date')::date)
  AND (sqlc.narg('to_date')::date IS NULL OR t.transfer_date <= sqlc.narg('to_date')::date)
ORDER BY t.transfer_date DESC, t.id DESC
LIMIT @lim OFFSET @off;

-- name: InsertAdjustment :one
INSERT INTO stock_adjustment (doc_no, fy, adj_date, location_id, reason, notes, created_by)
VALUES (@doc_no, @fy, @adj_date, @location_id, @reason, sqlc.narg('notes'), @created_by)
RETURNING id;

-- name: InsertAdjustmentLine :one
INSERT INTO stock_adjustment_line (adjustment_id, item_id, qty_change) VALUES (@adjustment_id, @item_id, @qty_change)
RETURNING id;

-- name: GetAdjustment :one
SELECT a.*, coalesce(u.full_name, '')::text AS created_by_name
FROM stock_adjustment a LEFT JOIN app_user u ON u.id = a.created_by
WHERE a.id = $1;

-- name: AdjustmentLines :many
SELECT al.adjustment_id, al.item_id, i.name AS item_name, al.qty_change
FROM stock_adjustment_line al JOIN item i ON i.id = al.item_id
WHERE al.adjustment_id = ANY(@ids::bigint[])
ORDER BY al.adjustment_id, al.id;

-- name: ListAdjustments :many
SELECT a.*, coalesce(u.full_name, '')::text AS created_by_name, count(*) OVER ()::bigint AS total
FROM stock_adjustment a LEFT JOIN app_user u ON u.id = a.created_by
WHERE (sqlc.narg('location_id')::smallint IS NULL OR a.location_id = sqlc.narg('location_id')::smallint)
  AND (sqlc.narg('from_date')::date IS NULL OR a.adj_date >= sqlc.narg('from_date')::date)
  AND (sqlc.narg('to_date')::date IS NULL OR a.adj_date <= sqlc.narg('to_date')::date)
ORDER BY a.adj_date DESC, a.id DESC
LIMIT @lim OFFSET @off;

-- name: StockMismatches :many
-- Nightly reconciliation: stock_balance must equal the sum of stock_movement.
SELECT sb.item_id, sb.location_id, sb.qty AS balance_qty, coalesce(m.net_qty, 0)::numeric AS movement_qty
FROM stock_balance sb
LEFT JOIN (
  SELECT item_id, location_id, sum(qty_in - qty_out) AS net_qty
  FROM stock_movement
  GROUP BY item_id, location_id
) m ON m.item_id = sb.item_id AND m.location_id = sb.location_id
WHERE sb.qty <> coalesce(m.net_qty, 0);
