-- name: ListItemIDs :many
-- Inventory grid: filter, sort and page; details are loaded by ItemsByIDs.
WITH base AS (
  SELECT i.id, i.name, c.name AS category_name, i.updated_at,
         coalesce(s.total, 0)::numeric AS stock_total,
         round(coalesce(s.total, 0) * i.avg_cost, 2)::numeric AS stock_value,
         (CASE WHEN coalesce(s.total, 0) <= 0 THEN 'out'
               WHEN coalesce(s.total, 0) <= i.reorder_level THEN 'low' ELSE 'ok' END)::text AS stock_status
  FROM item i
  JOIN category c ON c.id = i.category_id
  LEFT JOIN LATERAL (SELECT sum(sb.qty) AS total FROM stock_balance sb WHERE sb.item_id = i.id) s ON true
  WHERE (@include_inactive::bool OR i.is_active)
    AND (sqlc.narg('category_id')::int IS NULL OR i.category_id = sqlc.narg('category_id')::int)
    AND (sqlc.narg('location_id')::smallint IS NULL OR EXISTS (
          SELECT 1 FROM stock_balance b
           WHERE b.item_id = i.id AND b.location_id = sqlc.narg('location_id')::smallint AND b.qty > 0))
    AND NOT EXISTS (
          SELECT 1 FROM unnest(@terms::text[]) t
           WHERE lower(i.name || ' ' || coalesce(i.sku, '') || ' ' || c.name || ' ' || i.attributes::text)
                 NOT LIKE '%' || t || '%')
    AND NOT EXISTS (
          SELECT 1 FROM jsonb_each_text(@attrs::jsonb) f
           WHERE lower(i.attributes ->> f.key) IS DISTINCT FROM lower(f.value))
)
SELECT id, count(*) OVER ()::bigint AS total
FROM base
WHERE sqlc.narg('stock_status')::text IS NULL OR stock_status = sqlc.narg('stock_status')::text
ORDER BY
  CASE WHEN @sort::text = 'name'        AND NOT @descending::bool THEN name END ASC,
  CASE WHEN @sort::text = 'name'        AND @descending::bool     THEN name END DESC,
  CASE WHEN @sort::text = 'category'    AND NOT @descending::bool THEN category_name END ASC,
  CASE WHEN @sort::text = 'category'    AND @descending::bool     THEN category_name END DESC,
  CASE WHEN @sort::text = 'stock_total' AND NOT @descending::bool THEN stock_total END ASC,
  CASE WHEN @sort::text = 'stock_total' AND @descending::bool     THEN stock_total END DESC,
  CASE WHEN @sort::text = 'stock_value' AND NOT @descending::bool THEN stock_value END ASC,
  CASE WHEN @sort::text = 'stock_value' AND @descending::bool     THEN stock_value END DESC,
  CASE WHEN @sort::text = 'updated_at'  AND NOT @descending::bool THEN updated_at END ASC,
  CASE WHEN @sort::text = 'updated_at'  AND @descending::bool     THEN updated_at END DESC,
  name, id
LIMIT @lim OFFSET @off;

-- name: ItemsByIDs :many
SELECT i.id, i.category_id, i.name, i.attributes, i.sku, i.hsn_code, i.gst_rate, i.base_unit_id, i.reorder_level,
       i.avg_cost, i.last_purchase_rate, i.is_active, i.tally_name, i.updated_at,
       c.name AS category_name,
       u.name AS unit_name, u.symbol AS unit_symbol, u.uqc_code AS unit_uqc, u.decimals AS unit_decimals,
       coalesce((SELECT sum(sb.qty) FROM stock_balance sb WHERE sb.item_id = i.id), 0)::numeric AS stock_total
FROM item i
JOIN category c ON c.id = i.category_id
JOIN unit u ON u.id = i.base_unit_id
WHERE i.id = ANY(@ids::bigint[]);

-- name: ItemUnitsByItems :many
SELECT iu.item_id, iu.unit_id, iu.factor_to_base, u.symbol, u.name, u.decimals
FROM item_unit iu JOIN unit u ON u.id = iu.unit_id
WHERE iu.item_id = ANY(@ids::bigint[])
ORDER BY iu.item_id, iu.factor_to_base, u.symbol;

-- name: ItemStockByItems :many
SELECT i.id AS item_id, l.id AS location_id, l.name AS location_name, coalesce(sb.qty, 0)::numeric AS qty
FROM item i
CROSS JOIN location l
LEFT JOIN stock_balance sb ON sb.item_id = i.id AND sb.location_id = l.id
WHERE i.id = ANY(@ids::bigint[]) AND (l.is_active OR coalesce(sb.qty, 0) <> 0)
ORDER BY i.id, l.id;

-- name: InsertItem :one
INSERT INTO item (category_id, name, attributes, sku, hsn_code, gst_rate, base_unit_id, reorder_level, created_by)
VALUES (@category_id, @name, @attributes, sqlc.narg('sku'), @hsn_code, @gst_rate, @base_unit_id, @reorder_level,
        sqlc.narg('created_by'))
RETURNING id;

-- name: UpdateItem :execrows
UPDATE item SET category_id = @category_id, name = @name, attributes = @attributes, sku = sqlc.narg('sku'),
       hsn_code = @hsn_code, gst_rate = @gst_rate, base_unit_id = @base_unit_id, reorder_level = @reorder_level,
       updated_at = now()
WHERE id = @id;

-- name: SetItemActive :execrows
UPDATE item SET is_active = @is_active, updated_at = now() WHERE id = @id;

-- name: LockItem :one
SELECT * FROM item WHERE id = $1 FOR UPDATE;

-- name: ItemHasMovements :one
SELECT EXISTS (SELECT 1 FROM stock_movement WHERE item_id = $1)::bool AS has;

-- name: UpsertItemUnit :exec
INSERT INTO item_unit (item_id, unit_id, factor_to_base) VALUES (@item_id, @unit_id, @factor_to_base)
ON CONFLICT (item_id, unit_id) DO UPDATE SET factor_to_base = EXCLUDED.factor_to_base;

-- name: DeleteItemUnitsExcept :exec
DELETE FROM item_unit WHERE item_id = @item_id AND NOT (unit_id = ANY(@keep::int[]));

-- name: FindDuplicateItem :one
SELECT id FROM item
WHERE category_id = @category_id AND attributes = @attributes::jsonb AND id <> @exclude_id
LIMIT 1;

-- name: ItemMovements :many
WITH m AS (
  SELECT sm.*, sum(sm.qty_in - sm.qty_out) OVER (ORDER BY sm.id) AS running
  FROM stock_movement sm
  WHERE sm.item_id = @item_id
    AND (sqlc.narg('location_id')::smallint IS NULL OR sm.location_id = sqlc.narg('location_id')::smallint)
)
SELECT m.id, m.created_at, m.location_id, l.name AS location_name, m.kind, m.qty_in, m.qty_out,
       m.running::numeric AS balance, coalesce(m.unit_cost, 0)::numeric AS unit_cost, m.ref_type, m.ref_id,
       coalesce(fn_ref_no(m.ref_type, m.ref_id), '')::text AS ref_no,
       coalesce(u.full_name, '')::text AS user_name,
       count(*) OVER ()::bigint AS total
FROM m
JOIN location l ON l.id = m.location_id
LEFT JOIN app_user u ON u.id = m.created_by
WHERE (sqlc.narg('from_date')::date IS NULL OR m.created_at >= sqlc.narg('from_date')::date)
  AND (sqlc.narg('to_date')::date IS NULL OR m.created_at < sqlc.narg('to_date')::date + 1)
ORDER BY m.id
LIMIT @lim OFFSET @off;

-- name: LastRateToCustomer :one
SELECT bl.rate, u.symbol AS unit, coalesce(b.bill_no, '')::text AS bill_no, b.bill_date
FROM bill_line bl
JOIN bill b ON b.id = bl.bill_id
JOIN unit u ON u.id = bl.unit_id
WHERE bl.item_id = @item_id AND b.party_id = @party_id AND b.status = 'active'
  AND (sqlc.narg('unit_id')::int IS NULL OR bl.unit_id = sqlc.narg('unit_id')::int)
ORDER BY b.bill_date DESC, b.id DESC
LIMIT 1;

-- name: RecentRates :many
SELECT bl.rate, u.symbol AS unit, b.cust_name, b.bill_date
FROM bill_line bl
JOIN bill b ON b.id = bl.bill_id
JOIN unit u ON u.id = bl.unit_id
WHERE bl.item_id = @item_id AND b.status = 'active'
  AND (sqlc.narg('unit_id')::int IS NULL OR bl.unit_id = sqlc.narg('unit_id')::int)
ORDER BY b.bill_date DESC, b.id DESC
LIMIT 5;

-- name: StockIn :exec
SELECT fn_stock_in(@item_id::bigint, @location_id::smallint, @qty::numeric, sqlc.narg('cost')::numeric,
                   @kind::movement_kind, sqlc.narg('ref_type')::doc_kind, sqlc.narg('ref_id')::bigint,
                   sqlc.narg('ref_line')::bigint, sqlc.narg('user_id')::uuid);

-- name: StockOut :one
SELECT fn_stock_out(@item_id::bigint, @location_id::smallint, @qty::numeric, @kind::movement_kind,
                    @ref_type::doc_kind, @ref_id::bigint, sqlc.narg('ref_line')::bigint, @user_id::uuid)::numeric AS unit_cost;

-- name: RemoveCost :exec
SELECT fn_remove_cost(@item_id::bigint, @qty::numeric, @cost::numeric);

-- name: LockStockRows :many
SELECT sb.item_id, sb.location_id, sb.qty
FROM stock_balance sb
WHERE sb.item_id = ANY(@item_ids::bigint[])
ORDER BY sb.item_id, sb.location_id
FOR UPDATE;

-- name: StockRows :many
SELECT sb.item_id, sb.location_id, sb.qty
FROM stock_balance sb
WHERE sb.item_id = ANY(@item_ids::bigint[]);

-- name: AlertShortage :exec
SELECT fn_alert_shortage(@item_id::bigint, @location_id::smallint, @required::numeric, @user_id::uuid);

-- name: StockAtLocation :many
SELECT i.id, i.name, i.avg_cost, i.is_active, coalesce(sb.qty, 0)::numeric AS qty, u.symbol AS unit_symbol
FROM item i
JOIN unit u ON u.id = i.base_unit_id
LEFT JOIN stock_balance sb ON sb.item_id = i.id AND sb.location_id = @location_id::smallint
WHERE i.id = ANY(@ids::bigint[]);

-- name: GetItemByID :one
SELECT id FROM item WHERE id = $1;
