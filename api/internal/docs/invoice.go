package docs

import (
	"bytes"
	"strings"

	"github.com/go-pdf/fpdf"
	"github.com/skip2/go-qrcode"

	"billing-api/internal/api"
	"billing-api/internal/db/dbq"
)

var copyLabels = map[string]string{
	"original":   "ORIGINAL FOR RECIPIENT",
	"duplicate":  "DUPLICATE FOR TRANSPORTER",
	"triplicate": "TRIPLICATE FOR SUPPLIER",
}

// shopHeader prints the seller block and returns the y after it.
func (p *page) shopHeader(s dbq.ShopSetting, title, subtitle string) {
	p.SetFont("Helvetica", "B", 15)
	p.cell(120, 7, s.ShopName, "", "L", false)
	p.SetFont("Helvetica", "B", 12)
	p.cell(0, 7, title, "", "R", false)
	p.Ln(6)
	p.SetFont("Helvetica", "", 8.5)
	y := p.GetY()
	p.MultiCell(120, 4, p.T(s.Address), "", "L", false)
	if s.Gstin != "" {
		p.MultiCell(120, 4, p.T("GSTIN: "+s.Gstin+"   State code: "+s.StateCode), "", "L", false)
	}
	if s.Phone != nil {
		p.MultiCell(120, 4, p.T("Phone: "+*s.Phone), "", "L", false)
	}
	end := p.GetY()
	if subtitle != "" {
		p.SetXY(130, y)
		p.SetFont("Helvetica", "B", 8)
		p.cell(0, 4, subtitle, "", "R", false)
	}
	p.SetY(end + 2)
	p.Line(10, p.GetY(), 200, p.GetY())
	p.Ln(2)
}

func (p *page) kv(label, value string, w float64) {
	p.SetFont("Helvetica", "B", 8.5)
	p.cell(28, 4.5, label, "", "L", false)
	p.SetFont("Helvetica", "", 8.5)
	p.cell(w-28, 4.5, value, "", "L", false)
}

// qr draws the signed QR (e-invoice) at x,y with side mm.
func (p *page) qr(data string, x, y, side float64) {
	png, err := qrcode.Encode(data, qrcode.Medium, 512)
	if err != nil {
		return
	}
	name := "qr" + itoa(len(data))
	p.RegisterImageOptionsReader(name, fpdf.ImageOptions{ImageType: "PNG"}, bytes.NewReader(png))
	p.ImageOptions(name, x, y, side, side, false, fpdf.ImageOptions{ImageType: "PNG"}, 0, "")
}

func (p *page) watermark(text string) {
	p.SetFont("Helvetica", "B", 60)
	p.SetTextColor(220, 60, 60)
	p.SetAlpha(0.25, "Normal")
	p.TransformBegin()
	p.TransformRotate(35, 105, 150)
	p.Text(45, 160, text)
	p.TransformEnd()
	p.SetAlpha(1, "Normal")
	p.SetTextColor(0, 0, 0)
}

// Invoice renders a Cash or Account bill (FR-BL-06, FR-AB-01, FR-ED-05).
func Invoice(s dbq.ShopSetting, b api.Bill, copyKind string) ([]byte, error) {
	p := newPage("P")
	p.AddPage()
	title := "TAX INVOICE"
	if b.BillType == "account" {
		title = "TAX INVOICE - ACCOUNT"
	}
	label := copyLabels[copyKind]
	if label == "" {
		label = copyLabels["original"]
	}
	p.shopHeader(s, title, label)

	top := p.GetY()
	p.kv("Invoice no", deref(b.BillNo), 95)
	p.kv("Date", DMY(b.BillDate.T()), 95)
	p.Ln(4.5)
	p.kv("Place of supply", b.PlaceOfSupply, 95)
	p.kv("Vehicle no", deref(b.VehicleNo), 95)
	p.Ln(6)
	p.SetFont("Helvetica", "B", 9)
	p.cell(0, 5, "Bill to", "", "L", false)
	p.Ln(5)
	p.SetFont("Helvetica", "", 8.5)
	p.MultiCell(130, 4, p.T(b.Customer.Name), "", "L", false)
	if b.Customer.GSTIN != nil {
		p.MultiCell(130, 4, p.T("GSTIN: "+*b.Customer.GSTIN), "", "L", false)
	}
	if b.Customer.Address != nil {
		p.MultiCell(130, 4, p.T(*b.Customer.Address), "", "L", false)
	}
	if b.Customer.SiteAddress != nil {
		p.MultiCell(130, 4, p.T("Site: "+*b.Customer.SiteAddress), "", "L", false)
	}
	if b.Customer.Mobile != nil {
		p.MultiCell(130, 4, p.T("Mobile: "+*b.Customer.Mobile), "", "L", false)
	}
	if b.EInvoice.SignedQR != nil {
		p.qr(*b.EInvoice.SignedQR, 165, top, 34)
		p.SetY(maxf(p.GetY(), top+36))
		p.SetFont("Helvetica", "", 7)
		p.MultiCell(0, 3.5, p.T("IRN: "+deref(b.EInvoice.IRN)), "", "L", false)
		ack := "Ack no: " + deref(b.EInvoice.AckNo)
		if b.EInvoice.AckDate != nil {
			ack += "   Ack date: " + b.EInvoice.AckDate.Format("02-01-2006 15:04")
		}
		p.MultiCell(0, 3.5, p.T(ack), "", "L", false)
	}
	if b.EInvoice.EWBNo != nil {
		p.SetFont("Helvetica", "", 7)
		p.MultiCell(0, 3.5, p.T("E-way bill: "+*b.EInvoice.EWBNo), "", "L", false)
	}
	p.Ln(2)

	// Lines.
	cols := []struct {
		h     string
		w     float64
		align string
	}{{"#", 7, "C"}, {"Description", 55, "L"}, {"HSN/SAC", 16, "C"}, {"Qty", 16, "R"}, {"Unit", 11, "C"},
		{"Rate", 18, "R"}, {"Disc", 14, "R"}, {"Taxable", 20, "R"}, {"GST%", 10, "R"}, {"Total", 23, "R"}}
	p.SetFillColor(226, 232, 240)
	p.SetFont("Helvetica", "B", 7.5)
	for _, c := range cols {
		p.cell(c.w, 6, c.h, "1", c.align, true)
	}
	p.Ln(-1)
	p.SetFont("Helvetica", "", 7.5)
	row := func(vals []string) {
		for i, c := range cols {
			p.CellFormat(c.w, 5.5, p.fit(vals[i], c.w), "1", 0, c.align, false, 0, "")
		}
		p.Ln(-1)
	}
	n := 0
	for _, l := range b.Lines {
		n++
		disc := l.Qty.D().Mul(l.Rate.D()).Round(2).Sub(l.TaxableValue.D())
		row([]string{itoa(n), l.Description, l.HSNCode, l.Qty.D().StringFixed(3), l.Unit, INR(l.Rate.D()),
			INR(disc), INR(l.TaxableValue.D()), l.GSTRate, INR(l.LineTotal.D())})
	}
	for _, c := range b.Charges {
		n++
		tax := c.CGST.D().Add(c.SGST.D()).Add(c.IGST.D())
		row([]string{itoa(n), c.Description, deref(c.SACCode), "", "", "", "", INR(c.Amount.D()), c.GSTRate,
			INR(c.Amount.D().Add(tax))})
	}
	p.Ln(2)

	// Tax summary (left) and totals (right).
	y := p.GetY()
	p.SetFont("Helvetica", "B", 7.5)
	for _, h := range []string{"GST%", "Taxable", "CGST", "SGST", "IGST"} {
		p.cell(20, 5, h, "1", "C", true)
	}
	p.Ln(-1)
	p.SetFont("Helvetica", "", 7.5)
	for _, t := range b.Totals.TaxByRate {
		p.cell(20, 5, t.GSTRate, "1", "R", false)
		p.cell(20, 5, INR(t.Taxable.D()), "1", "R", false)
		p.cell(20, 5, INR(t.CGST.D()), "1", "R", false)
		p.cell(20, 5, INR(t.SGST.D()), "1", "R", false)
		p.cell(20, 5, INR(t.IGST.D()), "1", "R", false)
		p.Ln(-1)
	}
	left := p.GetY()
	p.SetY(y)
	tot := func(label, val string, bold bool) {
		p.SetX(130)
		style := ""
		if bold {
			style = "B"
		}
		p.SetFont("Helvetica", style, 8.5)
		p.cell(40, 5, label, "", "L", false)
		p.cell(30, 5, val, "", "R", false)
		p.Ln(5)
	}
	t := b.Totals
	if !t.BillDiscount.D().IsZero() {
		tot("Bill discount", INR(t.BillDiscount.D()), false)
	}
	tot("Taxable value", INR(t.TaxableTotal.D()), false)
	if !t.IGST.D().IsZero() {
		tot("IGST", INR(t.IGST.D()), false)
	} else {
		tot("CGST", INR(t.CGST.D()), false)
		tot("SGST", INR(t.SGST.D()), false)
	}
	tot("Round off", INR(t.RoundOff.D()), false)
	tot("Grand total (Rs.)", INR(t.GrandTotal.D()), true)
	if !t.AmountPaid.D().IsZero() {
		tot("Paid", INR(t.AmountPaid.D()), false)
		tot("Balance due", INR(t.BalanceDue.D()), true)
	}
	p.SetY(maxf(left, p.GetY()) + 2)
	p.SetFont("Helvetica", "B", 8.5)
	p.MultiCell(0, 4.5, p.T(t.AmountInWords), "", "L", false)
	p.Ln(2)
	p.footerBlock(s)
	if b.Status == "cancelled" {
		p.watermark("CANCELLED")
	}
	return p.bytes()
}

// footerBlock prints bank details, terms and the signature line.
func (p *page) footerBlock(s dbq.ShopSetting) {
	p.SetFont("Helvetica", "", 7.5)
	var bank []string
	if s.BankName != nil {
		bank = append(bank, "Bank: "+*s.BankName)
	}
	if s.BankAccountNo != nil {
		bank = append(bank, "A/c: "+*s.BankAccountNo)
	}
	if s.BankIfsc != nil {
		bank = append(bank, "IFSC: "+*s.BankIfsc)
	}
	if s.UpiID != nil {
		bank = append(bank, "UPI: "+*s.UpiID)
	}
	if len(bank) > 0 {
		p.MultiCell(120, 4, p.T(strings.Join(bank, "   ")), "", "L", false)
	}
	if s.InvoiceTerms != nil {
		p.MultiCell(120, 4, p.T("Terms: "+*s.InvoiceTerms), "", "L", false)
	}
	p.Ln(8)
	p.SetFont("Helvetica", "B", 8.5)
	p.cell(0, 5, "For "+s.ShopName, "", "R", false)
	p.Ln(10)
	p.SetFont("Helvetica", "", 7.5)
	p.cell(0, 4, "Authorised signatory", "", "R", false)
}

// CreditNote renders an approved (or pending) credit note (FR-RT-03).
func CreditNote(s dbq.ShopSetting, cn api.CreditNote) ([]byte, error) {
	p := newPage("P")
	p.AddPage()
	title := "CREDIT NOTE"
	if cn.Status != "approved" {
		title = "CREDIT NOTE (" + strings.ToUpper(cn.Status) + ")"
	}
	p.shopHeader(s, title, "")
	top := p.GetY()
	p.kv("Credit note no", deref(cn.CNNo), 95)
	p.kv("Date", DMY(cn.CNDate.T()), 95)
	p.Ln(4.5)
	p.kv("Against invoice", cn.BillNo, 95)
	p.kv("Customer", cn.PartyName, 95)
	p.Ln(4.5)
	p.kv("Reason", cn.Reason, 190)
	p.Ln(6)
	if cn.EInvoice.SignedQR != nil {
		p.qr(*cn.EInvoice.SignedQR, 165, top, 30)
		p.SetY(maxf(p.GetY(), top+32))
		p.SetFont("Helvetica", "", 7)
		p.MultiCell(0, 3.5, p.T("IRN: "+deref(cn.EInvoice.IRN)), "", "L", false)
	}
	cols := []struct {
		h string
		w float64
		a string
	}{{"#", 8, "C"}, {"Description", 80, "L"}, {"Qty", 22, "R"}, {"Unit", 15, "C"}, {"Rate", 25, "R"},
		{"Taxable", 25, "R"}, {"Restock", 15, "C"}}
	p.SetFillColor(226, 232, 240)
	p.SetFont("Helvetica", "B", 7.5)
	for _, c := range cols {
		p.cell(c.w, 6, c.h, "1", c.a, true)
	}
	p.Ln(-1)
	p.SetFont("Helvetica", "", 7.5)
	for i, l := range cn.Lines {
		restock := "No"
		if l.Restock {
			restock = "Yes"
		}
		vals := []string{itoa(i + 1), l.Description, l.Qty.D().StringFixed(3), l.Unit, INR(l.Rate.D()), INR(l.TaxableValue.D()), restock}
		for j, c := range cols {
			p.CellFormat(c.w, 5.5, p.fit(vals[j], c.w), "1", 0, c.a, false, 0, "")
		}
		p.Ln(-1)
	}
	p.Ln(2)
	t := cn.Totals
	for _, r := range [][2]string{{"Taxable value", INR(t.TaxableTotal.D())}, {"CGST", INR(t.CGST.D())},
		{"SGST", INR(t.SGST.D())}, {"IGST", INR(t.IGST.D())}, {"Total (Rs.)", INR(t.GrandTotal.D())}} {
		p.SetX(130)
		p.cell(40, 5, r[0], "", "L", false)
		p.cell(30, 5, r[1], "", "R", false)
		p.Ln(5)
	}
	p.SetFont("Helvetica", "B", 8.5)
	p.MultiCell(0, 4.5, p.T(t.AmountInWords), "", "L", false)
	p.Ln(4)
	p.footerBlock(s)
	return p.bytes()
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
