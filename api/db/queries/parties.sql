-- name: ListPartyIDs :many
WITH base AS (
  SELECT p.id, p.name, (p.opening_balance + coalesce(l.bal, 0))::numeric AS balance
  FROM party p
  LEFT JOIN LATERAL (SELECT sum(pl.debit - pl.credit) AS bal FROM party_ledger pl WHERE pl.party_id = p.id) l ON true
  WHERE (@include_inactive::bool OR p.is_active)
    AND (sqlc.narg('kind')::text IS NULL
         OR (sqlc.narg('kind')::text = 'customer' AND p.kind IN ('customer', 'both'))
         OR (sqlc.narg('kind')::text = 'supplier' AND p.kind IN ('supplier', 'both'))
         OR (sqlc.narg('kind')::text = 'both' AND p.kind = 'both'))
    AND (@q::text = '' OR p.name ILIKE '%' || @q::text || '%' OR p.mobile ILIKE '%' || @q::text || '%'
         OR p.gstin ILIKE '%' || @q::text || '%')
)
SELECT id, count(*) OVER ()::bigint AS total
FROM base
WHERE sqlc.narg('with_balance')::text IS NULL
   OR (sqlc.narg('with_balance')::text = 'any' AND balance <> 0)
   OR (sqlc.narg('with_balance')::text = 'receivable' AND balance > 0)
   OR (sqlc.narg('with_balance')::text = 'payable' AND balance < 0)
ORDER BY
  CASE WHEN @sort::text = 'name'    AND NOT @descending::bool THEN name END ASC,
  CASE WHEN @sort::text = 'name'    AND @descending::bool     THEN name END DESC,
  CASE WHEN @sort::text = 'balance' AND NOT @descending::bool THEN balance END ASC,
  CASE WHEN @sort::text = 'balance' AND @descending::bool     THEN balance END DESC,
  name, id
LIMIT @lim OFFSET @off;

-- name: PartiesByIDs :many
SELECT p.*, (p.opening_balance + coalesce((SELECT sum(pl.debit - pl.credit) FROM party_ledger pl
                                           WHERE pl.party_id = p.id), 0))::numeric AS balance
FROM party p
WHERE p.id = ANY(@ids::bigint[]);

-- name: SitesByParties :many
SELECT * FROM party_site WHERE party_id = ANY(@ids::bigint[]) ORDER BY party_id, id;

-- name: InsertParty :one
INSERT INTO party (kind, name, mobile, alt_phone, gstin, state_code, billing_address, pincode, credit_limit,
                   opening_balance, opening_date, notes)
VALUES (@kind, @name, sqlc.narg('mobile'), sqlc.narg('alt_phone'), sqlc.narg('gstin'), sqlc.narg('state_code'),
        sqlc.narg('billing_address'), sqlc.narg('pincode'), sqlc.narg('credit_limit'), @opening_balance,
        sqlc.narg('opening_date'), sqlc.narg('notes'))
RETURNING id;

-- name: UpdateParty :execrows
UPDATE party SET kind = @kind, name = @name, mobile = sqlc.narg('mobile'), alt_phone = sqlc.narg('alt_phone'),
       gstin = sqlc.narg('gstin'), state_code = sqlc.narg('state_code'), billing_address = sqlc.narg('billing_address'),
       pincode = sqlc.narg('pincode'), credit_limit = sqlc.narg('credit_limit'), opening_balance = @opening_balance,
       opening_date = sqlc.narg('opening_date'), notes = sqlc.narg('notes')
WHERE id = @id;

-- name: SetPartyActive :execrows
UPDATE party SET is_active = @is_active WHERE id = @id;

-- name: DeletePartySites :exec
DELETE FROM party_site WHERE party_id = $1;

-- name: InsertPartySite :exec
INSERT INTO party_site (party_id, label, address) VALUES (@party_id, @label, @address);

-- name: PartyLedgerOpening :one
SELECT (p.opening_balance + coalesce((SELECT sum(l.debit - l.credit) FROM party_ledger l
                                      WHERE l.party_id = p.id
                                        AND sqlc.narg('from_date')::date IS NOT NULL
                                        AND l.entry_date < sqlc.narg('from_date')::date), 0))::numeric AS opening
FROM party p
WHERE p.id = @party_id;

-- name: PartyLedgerEntries :many
SELECT l.id, l.entry_date, l.ref_type, l.ref_id, coalesce(fn_ref_no(l.ref_type, l.ref_id), '')::text AS ref_no,
       coalesce(l.narration, '')::text AS narration, l.debit, l.credit
FROM party_ledger l
WHERE l.party_id = @party_id
  AND (sqlc.narg('from_date')::date IS NULL OR l.entry_date >= sqlc.narg('from_date')::date)
  AND (sqlc.narg('to_date')::date IS NULL OR l.entry_date <= sqlc.narg('to_date')::date)
ORDER BY l.entry_date, l.id;

-- name: PartyBalance :one
SELECT (p.opening_balance + coalesce((SELECT sum(l.debit - l.credit) FROM party_ledger l
                                      WHERE l.party_id = p.id), 0))::numeric AS balance
FROM party p WHERE p.id = $1;

-- name: InsertLedger :exec
INSERT INTO party_ledger (party_id, entry_date, ref_type, ref_id, debit, credit, narration, is_reversal)
VALUES (@party_id, @entry_date, @ref_type, @ref_id, @debit, @credit, @narration, @is_reversal);

-- name: ReverseLedger :exec
-- Mirror every non-reversal posting of a document (cheque bounce, cancel).
INSERT INTO party_ledger (party_id, entry_date, ref_type, ref_id, debit, credit, narration, is_reversal)
SELECT pl.party_id, @entry_date::date, pl.ref_type, pl.ref_id, pl.credit, pl.debit,
       'Reversal: ' || coalesce(pl.narration, ''), true
FROM party_ledger pl
WHERE pl.ref_type = @ref_type::doc_kind AND pl.ref_id = @ref_id::bigint AND NOT pl.is_reversal;
