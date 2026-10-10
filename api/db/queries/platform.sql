-- name: ReserveIdempotencyKey :one
-- Inserted inside the document's transaction. A concurrent duplicate waits on
-- the primary key until the first commits, then sees ON CONFLICT and replays.
INSERT INTO api_idempotency (user_id, key, method, path, request_hash)
VALUES (@user_id, @key, @method, @path, @request_hash)
ON CONFLICT (user_id, key) DO NOTHING
RETURNING key;

-- name: GetIdempotencyKey :one
SELECT * FROM api_idempotency WHERE user_id = @user_id AND key = @key;

-- name: StoreIdempotentResponse :exec
UPDATE api_idempotency SET status_code = @status_code, response = @response WHERE user_id = @user_id AND key = @key;

-- name: PurgeIdempotencyKeys :execrows
DELETE FROM api_idempotency WHERE created_at < now() - interval '7 days';

-- name: InsertAudit :exec
INSERT INTO audit_log (user_id, action, entity, entity_id, before_data, after_data, ip_address)
VALUES (sqlc.narg('user_id'), @action, @entity, sqlc.narg('entity_id'), sqlc.narg('before_data'),
        sqlc.narg('after_data'), sqlc.narg('ip_address'));

-- name: ListAudit :many
SELECT a.id, u.full_name AS user_name, a.action, a.entity, a.entity_id, a.before_data, a.after_data,
       coalesce(host(a.ip_address), '')::text AS ip_address, a.created_at, count(*) OVER ()::bigint AS total
FROM audit_log a LEFT JOIN app_user u ON u.id = a.user_id
WHERE (sqlc.narg('user_id')::uuid IS NULL OR a.user_id = sqlc.narg('user_id')::uuid)
  AND (sqlc.narg('entity')::text IS NULL OR a.entity = sqlc.narg('entity')::text)
  AND (sqlc.narg('entity_id')::bigint IS NULL OR a.entity_id = sqlc.narg('entity_id')::bigint)
  AND (sqlc.narg('action')::text IS NULL OR a.action = sqlc.narg('action')::text)
  AND (sqlc.narg('from_date')::date IS NULL OR a.created_at >= sqlc.narg('from_date')::date)
  AND (sqlc.narg('to_date')::date IS NULL OR a.created_at < sqlc.narg('to_date')::date + 1)
ORDER BY a.id DESC
LIMIT @lim OFFSET @off;

-- name: ListNotifications :many
SELECT n.*, count(*) OVER ()::bigint AS total
FROM notification n
WHERE (n.user_id = @user_id OR (n.user_id IS NULL AND (n.for_role IS NULL OR n.for_role = @role::user_role)))
  AND (NOT @unread_only::bool OR NOT n.is_read)
ORDER BY n.created_at DESC, n.id DESC
LIMIT @lim OFFSET @off;

-- name: MarkNotificationRead :execrows
UPDATE notification n SET is_read = true
WHERE n.id = @id
  AND (n.user_id = @user_id OR (n.user_id IS NULL AND (n.for_role IS NULL OR n.for_role = @role::user_role)));

-- name: MarkAllNotificationsRead :exec
UPDATE notification n SET is_read = true
WHERE NOT n.is_read
  AND (n.user_id = @user_id OR (n.user_id IS NULL AND (n.for_role IS NULL OR n.for_role = @role::user_role)));

-- name: InsertNotification :exec
INSERT INTO notification (for_role, user_id, kind, title, body, ref_type, ref_id, item_id)
VALUES (sqlc.narg('for_role'), sqlc.narg('user_id'), @kind, @title, sqlc.narg('body'), sqlc.narg('ref_type'),
        sqlc.narg('ref_id'), sqlc.narg('item_id'));

-- name: InsertJob :one
INSERT INTO job (kind, params, created_by) VALUES (@kind, @params, sqlc.narg('created_by')) RETURNING *;

-- name: GetJob :one
SELECT * FROM job WHERE id = $1;

-- name: StartJob :exec
UPDATE job SET status = 'running', progress = greatest(progress, 1) WHERE id = $1;

-- name: JobProgress :exec
UPDATE job SET progress = @progress WHERE id = @id;

-- name: FinishJob :exec
UPDATE job SET status = 'succeeded', progress = 100, result = sqlc.narg('result'), result_path = sqlc.narg('result_path'),
       finished_at = now()
WHERE id = @id;

-- name: FailJob :exec
UPDATE job SET status = 'failed', error = @error, finished_at = now() WHERE id = @id;

-- name: SearchItems :many
SELECT i.id, i.name, coalesce((SELECT sum(qty) FROM stock_balance sb WHERE sb.item_id = i.id), 0)::numeric AS stock_total
FROM item i JOIN category c ON c.id = i.category_id
WHERE i.is_active
  AND NOT EXISTS (SELECT 1 FROM unnest(@terms::text[]) t
                   WHERE lower(i.name || ' ' || coalesce(i.sku, '') || ' ' || c.name || ' ' || i.attributes::text)
                         NOT LIKE '%' || t || '%')
ORDER BY i.name
LIMIT 8;

-- name: SearchParties :many
SELECT id, name, coalesce(mobile, '')::text AS mobile FROM party
WHERE is_active AND (name ILIKE '%' || @q::text || '%' OR mobile ILIKE '%' || @q::text || '%'
                     OR gstin ILIKE '%' || @q::text || '%')
ORDER BY name
LIMIT 8;

-- name: SearchBills :many
SELECT id, coalesce(bill_no, '')::text AS bill_no, bill_type, cust_name, bill_date FROM bill
WHERE status <> 'draft' AND (bill_no ILIKE '%' || @q::text || '%' OR cust_name ILIKE '%' || @q::text || '%')
ORDER BY bill_date DESC, id DESC
LIMIT 8;
