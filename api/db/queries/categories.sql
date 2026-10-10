-- name: ListCategories :many
SELECT c.*, (SELECT count(*) FROM item i WHERE i.category_id = c.id AND i.is_active)::bigint AS item_count
FROM category c
WHERE @include_inactive::bool OR c.is_active
ORDER BY c.name;

-- name: GetCategory :one
SELECT c.*, (SELECT count(*) FROM item i WHERE i.category_id = c.id AND i.is_active)::bigint AS item_count
FROM category c
WHERE c.id = $1;

-- name: InsertCategory :one
INSERT INTO category (name, parent_id, default_hsn, default_gst, description, tally_group)
VALUES (@name, sqlc.narg('parent_id'), sqlc.narg('default_hsn'), sqlc.narg('default_gst'),
        sqlc.narg('description'), sqlc.narg('tally_group'))
RETURNING id;

-- name: UpdateCategory :execrows
UPDATE category SET name = @name, parent_id = sqlc.narg('parent_id'), default_hsn = sqlc.narg('default_hsn'),
       default_gst = sqlc.narg('default_gst'), description = sqlc.narg('description'),
       tally_group = sqlc.narg('tally_group')
WHERE id = @id;

-- name: SetCategoryActive :execrows
UPDATE category SET is_active = @is_active WHERE id = @id;

-- name: CategoryUnits :many
SELECT cu.category_id, cu.is_default, u.id, u.name, u.symbol, u.uqc_code, u.decimals
FROM category_unit cu JOIN unit u ON u.id = cu.unit_id
WHERE cu.category_id = ANY(@category_ids::int[])
ORDER BY cu.category_id, cu.is_default DESC, u.name;

-- name: DeleteCategoryUnits :exec
DELETE FROM category_unit WHERE category_id = $1;

-- name: InsertCategoryUnit :exec
INSERT INTO category_unit (category_id, unit_id, is_default) VALUES (@category_id, @unit_id, @is_default);

-- name: CategoryAttributes :many
SELECT * FROM category_attribute
WHERE category_id = ANY(@category_ids::int[])
ORDER BY category_id, sort_order, id;

-- name: GetCategoryAttribute :one
SELECT * FROM category_attribute WHERE id = @id AND category_id = @category_id;

-- name: InsertCategoryAttribute :one
INSERT INTO category_attribute (category_id, code, label, data_type, options, value_unit, is_required, part_of_name, sort_order)
VALUES (@category_id, @code, @label, @data_type, sqlc.narg('options'), sqlc.narg('value_unit'), @is_required,
        @part_of_name, @sort_order)
RETURNING *;

-- name: UpdateCategoryAttribute :one
UPDATE category_attribute SET code = @code, label = @label, data_type = @data_type, options = sqlc.narg('options'),
       value_unit = sqlc.narg('value_unit'), is_required = @is_required, part_of_name = @part_of_name,
       sort_order = @sort_order
WHERE id = @id AND category_id = @category_id
RETURNING *;

-- name: DeleteCategoryAttribute :execrows
DELETE FROM category_attribute WHERE id = @id AND category_id = @category_id;

-- name: CountActiveItemsInCategory :one
SELECT count(*) FROM item WHERE category_id = $1 AND is_active;

-- name: CountItemsWithAttribute :one
SELECT count(*) FROM item WHERE category_id = @category_id AND (attributes ->> @code::text) IS NOT NULL;
