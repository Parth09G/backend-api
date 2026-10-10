package docs

import (
	"time"

	"github.com/shopspring/decimal"

	"billing-api/internal/api"
	"billing-api/internal/db/dbq"
	"billing-api/internal/num"
)

// Receipt renders a payment receipt / payment voucher (FR-CU-03).
func Receipt(s dbq.ShopSetting, pay api.Payment) ([]byte, error) {
	p := newPage("P")
	p.AddPage()
	title := "RECEIPT"
	if pay.Direction == "paid" {
		title = "PAYMENT VOUCHER"
	}
	if pay.Status == "cancelled" {
		title += " (CANCELLED)"
	}
	p.shopHeader(s, title, "")
	p.kv("Number", pay.DocNo, 95)
	p.kv("Date", DMY(pay.PayDate.T()), 95)
	p.Ln(6)
	who := "Received from"
	if pay.Direction == "paid" {
		who = "Paid to"
	}
	p.kv(who, pay.PartyName, 190)
	p.Ln(5)
	mode := pay.Mode
	if pay.Reference != nil {
		mode += "  ref " + *pay.Reference
	}
	if pay.ChequeNo != nil {
		mode += "  cheque " + *pay.ChequeNo
		if pay.BankName != nil {
			mode += " (" + *pay.BankName + ")"
		}
		if pay.ChequeStatus != nil {
			mode += " - " + *pay.ChequeStatus
		}
	}
	p.kv("Mode", mode, 190)
	p.Ln(5)
	p.kv("Amount", "Rs. "+INR(pay.Amount.D()), 190)
	p.Ln(5)
	p.SetFont("Helvetica", "B", 8.5)
	p.MultiCell(0, 5, p.T(num.Words(pay.Amount.D())), "", "L", false)
	p.Ln(3)
	if len(pay.Allocations) > 0 {
		p.SetFont("Helvetica", "B", 8)
		p.SetFillColor(226, 232, 240)
		p.cell(60, 6, "Against", "1", "L", true)
		p.cell(40, 6, "Amount", "1", "R", true)
		p.Ln(-1)
		p.SetFont("Helvetica", "", 8)
		for _, a := range pay.Allocations {
			p.cell(60, 5.5, a.RefNo, "1", "L", false)
			p.cell(40, 5.5, INR(a.Amount.D()), "1", "R", false)
			p.Ln(-1)
		}
		if !pay.Unallocated.D().IsZero() {
			p.cell(60, 5.5, "On account", "1", "L", false)
			p.cell(40, 5.5, INR(pay.Unallocated.D()), "1", "R", false)
			p.Ln(-1)
		}
	}
	p.Ln(6)
	p.footerBlock(s)
	return p.bytes()
}

// Statement renders a party's statement of account (FR-CU-02).
func Statement(s dbq.ShopSetting, l api.Ledger, from, to *time.Time) ([]byte, error) {
	p := newPage("P")
	p.AddPage()
	p.shopHeader(s, "STATEMENT OF ACCOUNT", "")
	period := "All dates"
	if from != nil || to != nil {
		period = ""
		if from != nil {
			period += "From " + DMY(*from) + " "
		}
		if to != nil {
			period += "To " + DMY(*to)
		}
	}
	p.kv("Party", l.Party.Name, 190)
	p.Ln(4.5)
	if l.Party.GSTIN != nil {
		p.kv("GSTIN", *l.Party.GSTIN, 190)
		p.Ln(4.5)
	}
	p.kv("Period", period, 190)
	p.Ln(7)
	cols := []struct {
		h string
		w float64
		a string
	}{{"Date", 20, "C"}, {"Document", 30, "L"}, {"Particulars", 62, "L"}, {"Debit", 26, "R"}, {"Credit", 26, "R"}, {"Balance", 26, "R"}}
	p.SetFillColor(226, 232, 240)
	p.SetFont("Helvetica", "B", 7.5)
	for _, c := range cols {
		p.cell(c.w, 6, c.h, "1", c.a, true)
	}
	p.Ln(-1)
	p.SetFont("Helvetica", "", 7.5)
	row := func(vals []string) {
		for i, c := range cols {
			p.CellFormat(c.w, 5.5, p.fit(vals[i], c.w), "1", 0, c.a, false, 0, "")
		}
		p.Ln(-1)
	}
	row([]string{"", "", "Opening balance", "", "", drcr(l.Opening.D())})
	dr, cr := decimal.Zero, decimal.Zero
	for _, e := range l.Entries {
		dr, cr = dr.Add(e.Debit.D()), cr.Add(e.Credit.D())
		row([]string{DMY(e.Date.T()), e.RefNo, e.Narration, blankZero(e.Debit.D()), blankZero(e.Credit.D()), drcr(e.Balance.D())})
	}
	p.SetFont("Helvetica", "B", 7.5)
	row([]string{"", "", "Closing balance", INR(dr), INR(cr), drcr(l.Closing.D())})
	p.Ln(4)
	p.SetFont("Helvetica", "", 7.5)
	p.MultiCell(0, 4, p.T("Dr = amount due to "+s.ShopName+"; Cr = amount payable by "+s.ShopName+"."), "", "L", false)
	return p.bytes()
}

func drcr(d decimal.Decimal) string {
	if d.IsNegative() {
		return INR(d.Abs()) + " Cr"
	}
	return INR(d) + " Dr"
}

func blankZero(d decimal.Decimal) string {
	if d.IsZero() {
		return ""
	}
	return INR(d)
}

// Table renders a generic report table (landscape) for /reports/{key}?format=pdf.
func Table(s dbq.ShopSetting, title, subtitle string, cols []api.ReportColumn, rows []map[string]any, totals map[string]any) ([]byte, error) {
	p := newPage("L")
	p.AddPage()
	p.SetFont("Helvetica", "B", 13)
	p.cell(0, 7, s.ShopName+" - "+title, "", "L", false)
	p.Ln(7)
	p.SetFont("Helvetica", "", 8)
	p.cell(0, 5, subtitle, "", "L", false)
	p.Ln(7)
	width := 277.0 / float64(len(cols))
	header := func() {
		p.SetFillColor(226, 232, 240)
		p.SetFont("Helvetica", "B", 7)
		for _, c := range cols {
			p.cell(width, 6, c.Label, "1", "C", true)
		}
		p.Ln(-1)
		p.SetFont("Helvetica", "", 7)
	}
	header()
	print := func(r map[string]any) {
		if p.GetY() > 190 {
			p.AddPage()
			header()
		}
		for _, c := range cols {
			align := "L"
			if c.Type == "money" || c.Type == "qty" || c.Type == "percent" {
				align = "R"
			}
			p.CellFormat(width, 5, p.fit(Format(r[c.Key], c.Type), width), "1", 0, align, false, 0, "")
		}
		p.Ln(-1)
	}
	for _, r := range rows {
		print(r)
	}
	if len(totals) > 0 {
		p.SetFont("Helvetica", "B", 7)
		print(totals)
	}
	return p.bytes()
}

// Format renders a report value for PDF/Excel by column type.
func Format(v any, typ string) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case api.Money:
		return INR(x.D())
	case api.Qty:
		return x.D().StringFixed(3)
	case api.Rate:
		return x.D().StringFixed(4)
	case api.Date:
		return DMY(x.T())
	case decimal.Decimal:
		if typ == "money" {
			return INR(x)
		}
		return x.String()
	case int64:
		return itoa64(x)
	case int:
		return itoa(x)
	case bool:
		if x {
			return "Yes"
		}
		return "No"
	case time.Time:
		return DMY(x)
	}
	return ""
}

func itoa64(n int64) string {
	if n < 0 {
		return "-" + itoa64(-n)
	}
	return itoa(int(n))
}
