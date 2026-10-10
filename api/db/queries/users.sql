-- name: GetAppUser :one
SELECT * FROM app_user WHERE id = $1;

-- name: ListAppUsers :many
SELECT * FROM app_user ORDER BY full_name;

-- name: InsertAppUser :one
INSERT INTO app_user (id, full_name, email, mobile, role)
VALUES (@id, @full_name, @email, sqlc.narg('mobile'), @role)
RETURNING *;

-- name: UpdateAppUser :one
UPDATE app_user SET
  full_name = coalesce(sqlc.narg('full_name'), full_name),
  mobile    = CASE WHEN @set_mobile::bool THEN sqlc.narg('mobile') ELSE mobile END,
  role      = coalesce(sqlc.narg('role')::user_role, role)
WHERE id = @id
RETURNING *;

-- name: SetAppUserActive :one
UPDATE app_user SET is_active = @is_active WHERE id = @id RETURNING *;

-- name: TouchAppUserLogin :exec
UPDATE app_user SET last_login_at = now(), last_session_id = sqlc.narg('session_id') WHERE id = @id;
