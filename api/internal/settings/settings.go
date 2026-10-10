// Package settings handles the shop profile, e-invoice credentials and Tally
// mapping (FR-ST-01).
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"billing-api/internal/api"
	"billing-api/internal/app"
	"billing-api/internal/audit"
	"billing-api/internal/auth"
	"billing-api/internal/db/dbq"
	"billing-api/internal/einvoice"
	"billing-api/internal/httpx"
	"billing-api/internal/num"
)

// SignedURLTTL is how long download links stay valid (API design 2.6).
const SignedURLTTL = 10 * time.Minute

type handler struct{ a *app.App }

func Routes(a *app.App) func(chi.Router) {
	h := handler{a}
	return func(r chi.Router) {
		r.Get("/", httpx.H(h.get))
		r.Get("/tally-mapping", httpx.H(h.getTally))
		r.Group(func(r chi.Router) {
			r.Use(auth.SuperAdmin)
			r.Put("/", httpx.H(h.put))
			r.Put("/logo", httpx.H(h.logo))
			r.Put("/einvoice-credentials", httpx.H(h.setCredentials))
			r.Post("/einvoice-credentials/test", httpx.H(h.testCredentials))
			r.Put("/tally-mapping", httpx.H(h.putTally))
		})
	}
}

// Load returns the shop settings or 409 when they have not been saved yet
// (bills need the shop's state code and GSTIN).
func Load(ctx context.Context, q *dbq.Queries) (dbq.ShopSetting, error) {
	s, err := q.GetSettings(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, httpx.InvalidState("complete the shop settings (name, address, state, GSTIN) first")
	}
	return s, err
}

func (h handler) toAPI(ctx context.Context, s dbq.ShopSetting) api.Settings {
	out := api.Settings{
		ShopName: s.ShopName, Address: s.Address, StateCode: s.StateCode, GSTIN: s.Gstin,
		Phone: deref(s.Phone), Email: deref(s.Email), BankName: deref(s.BankName),
		BankAccountNo: deref(s.BankAccountNo), BankIFSC: deref(s.BankIfsc), UPIID: deref(s.UpiID),
		InvoiceTerms: deref(s.InvoiceTerms), PricesIncludeGST: s.PricesIncludeGst,
		DefaultChargeGST: num.Trim(s.DefaultChargeGst), EInvoiceEnabled: s.EinvoiceEnabled,
		NumberPrefixes: map[string]string{},
	}
	_ = json.Unmarshal(s.NumberPrefixes, &out.NumberPrefixes)
	_ = json.Unmarshal(s.NotificationChannels, &out.NotificationChannels)
	if s.LogoPath != nil && h.a.Storage != nil {
		if url, _, err := h.a.Storage.SignedURL(ctx, *s.LogoPath, SignedURLTTL); err == nil {
			out.LogoURL = &url
		} else {
			slog.WarnContext(ctx, "logo signed url", "err", err)
		}
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (h handler) get(w http.ResponseWriter, r *http.Request) error {
	s, err := h.a.Q.GetSettings(r.Context())
	if errors.Is(err, pgx.ErrNoRows) {
		// Not set up yet: return an empty profile with the defaults.
		httpx.JSON(w, http.StatusOK, api.Settings{DefaultChargeGST: "18", EInvoiceEnabled: true, NumberPrefixes: map[string]string{}})
		return nil
	}
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, h.toAPI(r.Context(), s))
	return nil
}

// put replaces the profile; optional fields left out keep their value.
func (h handler) put(w http.ResponseWriter, r *http.Request) error {
	var in api.SettingsInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	var saved dbq.ShopSetting
	err := h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		cur, err := q.GetSettings(ctx)
		exists := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if !exists {
			cur = dbq.ShopSetting{DefaultChargeGst: decimal.NewFromInt(18), EinvoiceEnabled: true,
				NumberPrefixes: []byte("{}"), NotificationChannels: []byte("{}")}
		}
		p := dbq.UpsertSettingsParams{
			ShopName: in.ShopName, Address: in.Address, StateCode: in.StateCode, Gstin: in.GSTIN,
			Phone: pick(in.Phone, cur.Phone), Email: pick(in.Email, cur.Email), BankName: pick(in.BankName, cur.BankName),
			BankAccountNo: pick(in.BankAccountNo, cur.BankAccountNo), BankIfsc: pick(in.BankIFSC, cur.BankIfsc),
			UpiID: pick(in.UPIID, cur.UpiID), InvoiceTerms: pick(in.InvoiceTerms, cur.InvoiceTerms),
			PricesIncludeGst: cur.PricesIncludeGst, DefaultChargeGst: cur.DefaultChargeGst,
			EinvoiceEnabled: cur.EinvoiceEnabled, NumberPrefixes: cur.NumberPrefixes,
			NotificationChannels: cur.NotificationChannels,
		}
		if in.PricesIncludeGST != nil {
			p.PricesIncludeGst = *in.PricesIncludeGST
		}
		if in.DefaultChargeGST != nil {
			p.DefaultChargeGst = num.Parse(*in.DefaultChargeGST)
		}
		if in.EInvoiceEnabled != nil {
			p.EinvoiceEnabled = *in.EInvoiceEnabled
		}
		if in.NumberPrefixes != nil {
			p.NumberPrefixes, _ = json.Marshal(in.NumberPrefixes)
		}
		if in.NotificationChannels != nil {
			p.NotificationChannels, _ = json.Marshal(in.NotificationChannels)
		}
		saved, err = q.UpsertSettings(ctx, p)
		if err != nil {
			return err
		}
		var before any
		if exists {
			before = h.toAPI(ctx, cur)
		}
		return audit.Log(ctx, q, "settings", "shop_settings", audit.ID(1), before, h.toAPI(ctx, saved))
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, h.toAPI(ctx, saved))
	return nil
}

// pick keeps the current value when the field was left out; "" clears it.
func pick(in, cur *string) *string {
	if in == nil {
		return cur
	}
	if *in == "" {
		return nil
	}
	return in
}

// logo stores a PNG/JPG (max 1 MB) in the private bucket.
func (h handler) logo(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	if _, err := Load(ctx, h.a.Q); err != nil {
		return err
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20+4096)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		return httpx.Validation("file", "upload a PNG or JPG of at most 1 MB as multipart field \"file\"")
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		return httpx.Validation("file", "is required")
	}
	defer f.Close()
	if hdr.Size > 1<<20 {
		return httpx.Validation("file", "must be at most 1 MB")
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	ct := http.DetectContentType(data)
	ext := map[string]string{"image/png": "png", "image/jpeg": "jpg"}[ct]
	if ext == "" {
		return httpx.Validation("file", "must be a PNG or JPG image")
	}
	path := "settings/logo." + ext
	if err := h.a.Storage.Upload(ctx, path, ct, data); err != nil {
		return err
	}
	var s dbq.ShopSetting
	err = h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		if _, err := q.SetLogoPath(ctx, &path); err != nil {
			return err
		}
		if err := audit.Log(ctx, q, "settings", "shop_settings", audit.ID(1), nil, map[string]string{"logo_path": path}); err != nil {
			return err
		}
		s, err = q.GetSettings(ctx)
		return err
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, h.toAPI(ctx, s))
	return nil
}

func (h handler) setCredentials(w http.ResponseWriter, r *http.Request) error {
	var in api.EInvoiceCredentials
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	if _, err := Load(ctx, h.a.Q); err != nil {
		return err
	}
	secret, err := json.Marshal(einvoice.Secret{Password: in.Password, ClientSecret: deref(in.ClientSecret)})
	if err != nil {
		return err
	}
	sealed, err := h.a.Secrets.Seal(secret)
	if err != nil {
		return httpx.InvalidState("APP_ENCRYPTION_KEY is not configured on the server; credentials cannot be stored")
	}
	err = h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		if _, err := q.SetEInvoiceCredentials(ctx, dbq.SetEInvoiceCredentialsParams{
			Provider: &in.Provider, Environment: &in.Environment, Username: &in.Username, ClientID: in.ClientID, Secret: sealed,
		}); err != nil {
			return err
		}
		// Never log the secrets themselves.
		return audit.Log(ctx, q, "settings", "einvoice_credentials", nil, nil,
			map[string]string{"provider": in.Provider, "environment": in.Environment, "username": in.Username})
	})
	if err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}

func (h handler) testCredentials(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s, err := Load(ctx, h.a.Q)
	if err != nil {
		return err
	}
	p, err := einvoice.ProviderFor(h.a, s)
	if err != nil {
		httpx.JSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
		return nil
	}
	if err := p.Authenticate(ctx); err != nil {
		httpx.JSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
		return nil
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true, "message": "login to " + p.Name() + " succeeded"})
	return nil
}

func (h handler) getTally(w http.ResponseWriter, r *http.Request) error {
	var m api.TallyMapping
	s, err := h.a.Q.GetSettings(r.Context())
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil {
		_ = json.Unmarshal(s.TallyMapping, &m)
	}
	httpx.JSON(w, http.StatusOK, m)
	return nil
}

func (h handler) putTally(w http.ResponseWriter, r *http.Request) error {
	var in api.TallyMapping
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	b, _ := json.Marshal(in)
	err := h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		n, err := q.SetTallyMapping(ctx, b)
		if err != nil {
			return err
		}
		if n == 0 {
			return httpx.InvalidState("complete the shop settings first")
		}
		return audit.Log(ctx, q, "settings", "tally_mapping", nil, nil, in)
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, in)
	return nil
}
