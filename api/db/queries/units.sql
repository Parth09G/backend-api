-- name: ListUnits :many
SELECT * FROM unit ORDER BY name;

-- name: GetUnit :one
SELECT * FROM unit WHERE id = $1;

-- name: InsertUnit :one
INSERT INTO unit (name, symbol, uqc_code, decimals)
VALUES (@name, @symbol, sqlc.narg('uqc_code'), @decimals)
RETURNING *;

-- name: UpdateUnit :one
UPDATE unit SET name = @name, symbol = @symbol, uqc_code = sqlc.narg('uqc_code'), decimals = @decimals
WHERE id = @id
RETURNING *;
