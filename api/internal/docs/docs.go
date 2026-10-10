// Package docs renders PDFs (invoices, credit notes, receipts, statements,
// report tables) and publishes them to the private Storage bucket behind a
// short-lived signed link (API design 2.6).
//
// PDFs use the core Helvetica font (cp1252), so the rupee sign is written
// as "Rs." and amounts use Indian digit grouping (1,00,000.00).
package docs

import (
	"bytes"
	"context"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/shopspring/decimal"

	"billing-api/internal/api"
	"billing-api/internal/app"
)

// LinkTTL is how long a download link stays valid.
const LinkTTL = 10 * time.Minute

// Publish uploads a PDF and returns a signed download link.
func Publish(ctx context.Context, a *app.App, path string, pdf []byte) (api.SignedURL, error) {
	return PublishFile(ctx, a, path, "application/pdf", pdf)
}

// PublishFile uploads any file and returns a signed download link.
func PublishFile(ctx context.Context, a *app.App, path, contentType string, data []byte) (api.SignedURL, error) {
	if err := a.Storage.Upload(ctx, path, contentType, data); err != nil {
		return api.SignedURL{}, err
	}
	url, exp, err := a.Storage.SignedURL(ctx, path, LinkTTL)
	if err != nil {
		return api.SignedURL{}, err
	}
	return api.SignedURL{URL: url, ExpiresAt: exp}, nil
}

// INR formats an amount with Indian digit grouping: 148450.5 -> 1,48,450.50.
func INR(d decimal.Decimal) string {
	s := d.Abs().StringFixed(2)
	intPart, frac, _ := strings.Cut(s, ".")
	var b strings.Builder
	if n := len(intPart); n > 3 {
		head, tail := intPart[:n-3], intPart[n-3:]
		for i, c := range head {
			if i > 0 && (len(head)-i)%2 == 0 {
				b.WriteByte(',')
			}
			b.WriteRune(c)
		}
		b.WriteByte(',')
		b.WriteString(tail)
	} else {
		b.WriteString(intPart)
	}
	out := b.String() + "." + frac
	if d.IsNegative() {
		out = "-" + out
	}
	return out
}

// DMY formats a date as DD-MM-YYYY (NFR-06).
func DMY(t time.Time) string { return t.Format("02-01-2006") }

// page is a thin wrapper over fpdf with UTF-8 to cp1252 translation.
type page struct {
	*fpdf.Fpdf
	tr func(string) string
}

func newPage(orientation string) *page {
	f := fpdf.New(orientation, "mm", "A4", "")
	f.SetMargins(10, 10, 10)
	f.SetAutoPageBreak(true, 14)
	f.SetCreator("Billing & Inventory", true)
	f.AliasNbPages("{nb}")
	p := &page{Fpdf: f, tr: f.UnicodeTranslatorFromDescriptor("")}
	f.SetFooterFunc(func() {
		f.SetY(-10)
		f.SetFont("Helvetica", "I", 7)
		f.SetTextColor(120, 120, 120)
		f.CellFormat(0, 4, p.tr("Page "+itoa(f.PageNo())+" of {nb}"), "", 0, "R", false, 0, "")
		f.SetTextColor(0, 0, 0)
	})
	return p
}

// T translates text for the core font; the rupee sign becomes "Rs.".
func (p *page) T(s string) string {
	return p.tr(strings.ReplaceAll(s, "₹", "Rs."))
}

func (p *page) cell(w, h float64, txt, border, align string, fill bool) {
	p.CellFormat(w, h, p.T(txt), border, 0, align, fill, 0, "")
}

func (p *page) bytes() ([]byte, error) {
	var buf bytes.Buffer
	if err := p.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}

// fit shortens text to a column width.
func (p *page) fit(s string, w float64) string {
	s = p.T(s)
	for len(s) > 1 && p.GetStringWidth(s) > w-1.5 {
		s = s[:len(s)-1]
	}
	return s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
