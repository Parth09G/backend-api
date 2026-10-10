-- name: InsertEInvoiceLog :exec
INSERT INTO einvoice_log (ref_type, ref_id, action, request, response, success, error_code)
VALUES (@ref_type, @ref_id, @action, sqlc.narg('request'), sqlc.narg('response'), @success, sqlc.narg('error_code'));

-- name: SetBillIRN :exec
UPDATE bill SET einv_status = 'generated', irn = @irn, ack_no = @ack_no, ack_date = @ack_date, signed_qr = @signed_qr,
       einv_error = NULL, updated_at = now()
WHERE id = @id;

-- name: SetBillIRNCancelled :exec
UPDATE bill SET einv_status = 'cancelled', einv_error = NULL, updated_at = now() WHERE id = @id;

-- name: ClearBillIRN :exec
-- Before regenerating after an edit inside the 24-hour window.
UPDATE bill SET einv_status = 'pending', irn = NULL, ack_no = NULL, ack_date = NULL, signed_qr = NULL,
       einv_error = NULL, updated_at = now()
WHERE id = @id;

-- name: SetBillEWB :exec
UPDATE bill SET ewb_no = @ewb_no, ewb_valid_until = sqlc.narg('ewb_valid_until'), vehicle_no = @vehicle_no,
       einv_error = NULL, updated_at = now()
WHERE id = @id;

-- name: SetBillEInvoiceError :exec
UPDATE bill SET einv_status = @einv_status, einv_error = @einv_error, updated_at = now() WHERE id = @id;

-- name: SetCreditNoteIRN :exec
UPDATE credit_note SET einv_status = 'generated', irn = @irn, ack_no = @ack_no, ack_date = @ack_date,
       signed_qr = @signed_qr, einv_error = NULL
WHERE id = @id;

-- name: SetCreditNoteEInvoiceError :exec
UPDATE credit_note SET einv_status = @einv_status, einv_error = @einv_error WHERE id = @id;

-- name: SetCreditNoteIRNCancelled :exec
UPDATE credit_note SET einv_status = 'cancelled', einv_error = NULL WHERE id = @id;

-- name: UnitUQCs :many
SELECT id, coalesce(uqc_code, 'OTH')::text AS uqc FROM unit WHERE id = ANY(@ids::int[]);
