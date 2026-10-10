-- name: GetSettings :one
SELECT * FROM shop_settings WHERE id = 1;

-- name: UpsertSettings :one
INSERT INTO shop_settings (id, shop_name, address, state_code, gstin, phone, email, bank_name, bank_account_no,
                           bank_ifsc, upi_id, invoice_terms, prices_include_gst, default_charge_gst, einvoice_enabled,
                           number_prefixes, notification_channels, updated_at)
VALUES (1, @shop_name, @address, @state_code, @gstin, sqlc.narg('phone'), sqlc.narg('email'), sqlc.narg('bank_name'),
        sqlc.narg('bank_account_no'), sqlc.narg('bank_ifsc'), sqlc.narg('upi_id'), sqlc.narg('invoice_terms'),
        @prices_include_gst, @default_charge_gst, @einvoice_enabled, @number_prefixes, @notification_channels, now())
ON CONFLICT (id) DO UPDATE SET
  shop_name = EXCLUDED.shop_name, address = EXCLUDED.address, state_code = EXCLUDED.state_code,
  gstin = EXCLUDED.gstin, phone = EXCLUDED.phone, email = EXCLUDED.email, bank_name = EXCLUDED.bank_name,
  bank_account_no = EXCLUDED.bank_account_no, bank_ifsc = EXCLUDED.bank_ifsc, upi_id = EXCLUDED.upi_id,
  invoice_terms = EXCLUDED.invoice_terms, prices_include_gst = EXCLUDED.prices_include_gst,
  default_charge_gst = EXCLUDED.default_charge_gst, einvoice_enabled = EXCLUDED.einvoice_enabled,
  number_prefixes = EXCLUDED.number_prefixes, notification_channels = EXCLUDED.notification_channels,
  updated_at = now()
RETURNING *;

-- name: SetLogoPath :execrows
UPDATE shop_settings SET logo_path = @logo_path, updated_at = now() WHERE id = 1;

-- name: SetEInvoiceCredentials :execrows
UPDATE shop_settings SET einv_provider = @provider, einv_environment = @environment, einv_username = @username,
       einv_client_id = sqlc.narg('client_id'), einv_secret = @secret, updated_at = now()
WHERE id = 1;

-- name: SetTallyMapping :execrows
UPDATE shop_settings SET tally_mapping = @tally_mapping, updated_at = now() WHERE id = 1;
