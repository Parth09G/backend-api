-- name: ListLocations :many
SELECT * FROM location ORDER BY id;

-- name: GetLocation :one
SELECT * FROM location WHERE id = $1;

-- name: InsertLocation :one
INSERT INTO location (name, kind, address, is_default_billing)
VALUES (@name, @kind, sqlc.narg('address'), @is_default_billing)
RETURNING *;

-- name: UpdateLocation :one
UPDATE location SET name = @name, kind = @kind, address = sqlc.narg('address'),
       is_default_billing = @is_default_billing, is_active = @is_active
WHERE id = @id
RETURNING *;

-- name: ClearDefaultBilling :exec
UPDATE location SET is_default_billing = false WHERE is_default_billing AND id <> @keep_id;

-- name: DefaultBillingLocation :one
SELECT * FROM location WHERE is_default_billing AND is_active ORDER BY id LIMIT 1;
