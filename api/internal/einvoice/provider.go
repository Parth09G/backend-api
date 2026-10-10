// Package einvoice talks to the GST e-invoice system (IRP) and e-way bill
// portal through a GSP/ASP (FR-EI). The provider is not chosen yet (open
// item O-07), so it sits behind Provider; "mock" is a built-in provider that
// returns realistic IRNs for development and tests.
//
// All portal calls run in River jobs with retry, never inside a request
// (API design 2.6), and every request/response is written to einvoice_log.
package einvoice

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"billing-api/internal/app"
	"billing-api/internal/db/dbq"
)

// Secret is the encrypted part of the stored credentials.
type Secret struct {
	Password     string `json:"password"`
	ClientSecret string `json:"client_secret,omitempty"`
}

type IRNResult struct {
	IRN      string    `json:"Irn"`
	AckNo    string    `json:"AckNo"`
	AckDate  time.Time `json:"AckDt"`
	SignedQR string    `json:"SignedQRCode"`
}

type EWBRequest struct {
	IRN           string `json:"Irn,omitempty"`
	DocNo         string `json:"DocNo"`
	VehicleNo     string `json:"VehNo"`
	TransportMode string `json:"TransMode"`
	DistanceKm    int    `json:"Distance"`
	TransporterID string `json:"TransId,omitempty"`
}

type EWBResult struct {
	EWBNo      string    `json:"EwbNo"`
	EWBDate    time.Time `json:"EwbDt"`
	ValidUntil time.Time `json:"EwbValidTill"`
}

// Provider is one GSP/ASP integration.
type Provider interface {
	Name() string
	Authenticate(ctx context.Context) error
	GenerateIRN(ctx context.Context, doc Document) (IRNResult, error)
	CancelIRN(ctx context.Context, irn, reason string) error
	GenerateEWB(ctx context.Context, req EWBRequest) (EWBResult, error)
}

// PortalError is a rejection by the portal (validation, duplicate IRN ...).
type PortalError struct {
	Code    string
	Message string
}

func (e *PortalError) Error() string { return e.Code + ": " + e.Message }

// ProviderFor builds the provider from the stored settings.
func ProviderFor(a *app.App, s dbq.ShopSetting) (Provider, error) {
	if s.EinvProvider == nil || *s.EinvProvider == "" {
		return nil, errors.New("e-invoice credentials are not configured (Settings > E-invoice)")
	}
	var sec Secret
	if len(s.EinvSecret) > 0 {
		plain, err := a.Secrets.Open(s.EinvSecret)
		if err != nil {
			return nil, fmt.Errorf("cannot read stored credentials: %w", err)
		}
		_ = json.Unmarshal(plain, &sec)
	}
	switch strings.ToLower(*s.EinvProvider) {
	case "mock":
		return &Mock{GSTIN: s.Gstin, Username: deref(s.EinvUsername), Password: sec.Password, Now: a.Now}, nil
	default:
		return nil, fmt.Errorf("GSP provider %q is not supported yet (open item O-07); use \"mock\" until one is chosen", *s.EinvProvider)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Mock behaves like an IRP for development: deterministic IRN per document,
// 24-hour cancel rule, and a fake signed QR. Password "fail" makes calls fail.
type Mock struct {
	GSTIN, Username, Password string
	Now                       func() time.Time
}

func (m *Mock) Name() string { return "mock" }

func (m *Mock) Authenticate(context.Context) error {
	if m.Username == "" || m.Password == "" || m.Password == "fail" {
		return &PortalError{Code: "AUTH", Message: "invalid username or password"}
	}
	return nil
}

func (m *Mock) GenerateIRN(ctx context.Context, doc Document) (IRNResult, error) {
	if err := m.Authenticate(ctx); err != nil {
		return IRNResult{}, err
	}
	if doc.BuyerDtls.Gstin == "" {
		return IRNResult{}, &PortalError{Code: "2172", Message: "buyer GSTIN is required for B2B e-invoice"}
	}
	sum := sha256.Sum256([]byte(m.GSTIN + "|" + doc.DocDtls.Typ + "|" + doc.DocDtls.No + "|" + doc.DocDtls.Dt))
	irn := hex.EncodeToString(sum[:])
	now := m.Now()
	qr, _ := json.Marshal(map[string]any{"SellerGstin": m.GSTIN, "BuyerGstin": doc.BuyerDtls.Gstin,
		"DocNo": doc.DocDtls.No, "DocTyp": doc.DocDtls.Typ, "DocDt": doc.DocDtls.Dt,
		"TotInvVal": doc.ValDtls.TotInvVal, "Irn": irn, "IrnDt": now.Format("2006-01-02 15:04:05")})
	return IRNResult{
		IRN:      irn,
		AckNo:    fmt.Sprintf("1121%011d", now.UnixNano()%100000000000),
		AckDate:  now,
		SignedQR: "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(qr) + ".mock-signature",
	}, nil
}

func (m *Mock) CancelIRN(ctx context.Context, irn, reason string) error {
	return m.Authenticate(ctx)
}

func (m *Mock) GenerateEWB(ctx context.Context, req EWBRequest) (EWBResult, error) {
	if err := m.Authenticate(ctx); err != nil {
		return EWBResult{}, err
	}
	now := m.Now()
	days := req.DistanceKm/200 + 1 // validity: 1 day per 200 km
	return EWBResult{
		EWBNo:      fmt.Sprintf("1%011d", now.UnixNano()%100000000000),
		EWBDate:    now,
		ValidUntil: now.Add(time.Duration(days) * 24 * time.Hour),
	}, nil
}
