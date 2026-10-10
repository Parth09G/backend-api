package einvoice

import (
	"context"
	"regexp"
	"strings"

	"github.com/shopspring/decimal"

	"billing-api/internal/db/dbq"
)

// Document is the NIC e-invoice schema (INV-01, v1.1) subset we send.
type Document struct {
	Version    string    `json:"Version"`
	TranDtls   TranDtls  `json:"TranDtls"`
	DocDtls    DocDtls   `json:"DocDtls"`
	SellerDtls Party     `json:"SellerDtls"`
	BuyerDtls  Party     `json:"BuyerDtls"`
	ItemList   []DocItem `json:"ItemList"`
	ValDtls    ValDtls   `json:"ValDtls"`
	RefDtls    *RefDtls  `json:"RefDtls,omitempty"`
}

type TranDtls struct {
	TaxSch string `json:"TaxSch"`
	SupTyp string `json:"SupTyp"`
}

type DocDtls struct {
	Typ string `json:"Typ"` // INV | CRN
	No  string `json:"No"`
	Dt  string `json:"Dt"` // dd/mm/yyyy
}

type Party struct {
	Gstin string `json:"Gstin"`
	LglNm string `json:"LglNm"`
	Pos   string `json:"Pos,omitempty"`
	Addr1 string `json:"Addr1"`
	Loc   string `json:"Loc"`
	Pin   int    `json:"Pin"`
	Stcd  string `json:"Stcd"`
}

type DocItem struct {
	SlNo       string          `json:"SlNo"`
	PrdDesc    string          `json:"PrdDesc"`
	IsServc    string          `json:"IsServc"`
	HsnCd      string          `json:"HsnCd"`
	Qty        decimal.Decimal `json:"Qty"`
	Unit       string          `json:"Unit"`
	UnitPrice  decimal.Decimal `json:"UnitPrice"`
	TotAmt     decimal.Decimal `json:"TotAmt"`
	Discount   decimal.Decimal `json:"Discount"`
	AssAmt     decimal.Decimal `json:"AssAmt"`
	GstRt      decimal.Decimal `json:"GstRt"`
	IgstAmt    decimal.Decimal `json:"IgstAmt"`
	CgstAmt    decimal.Decimal `json:"CgstAmt"`
	SgstAmt    decimal.Decimal `json:"SgstAmt"`
	TotItemVal decimal.Decimal `json:"TotItemVal"`
}

type ValDtls struct {
	AssVal    decimal.Decimal `json:"AssVal"`
	CgstVal   decimal.Decimal `json:"CgstVal"`
	SgstVal   decimal.Decimal `json:"SgstVal"`
	IgstVal   decimal.Decimal `json:"IgstVal"`
	RndOffAmt decimal.Decimal `json:"RndOffAmt"`
	TotInvVal decimal.Decimal `json:"TotInvVal"`
}

type RefDtls struct {
	PrecDocDtls []DocDtls `json:"PrecDocDtls"`
}

var rePin = regexp.MustCompile(`\b[1-9][0-9]{5}\b`)

func pin(addr string) int {
	m := rePin.FindString(addr)
	n := 0
	for _, c := range m {
		n = n*10 + int(c-'0')
	}
	return n
}

func seller(s dbq.ShopSetting) Party {
	return Party{Gstin: strings.TrimSpace(s.Gstin), LglNm: s.ShopName, Addr1: trim100(s.Address), Loc: "-",
		Pin: pin(s.Address), Stcd: s.StateCode}
}

func trim100(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 100 {
		return s[:100]
	}
	if s == "" {
		return "-"
	}
	return s
}

func ddmmyyyy(t interface{ Format(string) string }) string { return t.Format("02/01/2006") }

// BillDocument builds the payload for a bill.
func BillDocument(ctx context.Context, q *dbq.Queries, s dbq.ShopSetting, b dbq.GetBillRow) (Document, error) {
	lines, err := q.BillLines(ctx, b.ID)
	if err != nil {
		return Document{}, err
	}
	charges, err := q.BillCharges(ctx, b.ID)
	if err != nil {
		return Document{}, err
	}
	uqc, err := uqcs(ctx, q, lines)
	if err != nil {
		return Document{}, err
	}
	addr := ""
	if b.CustAddress != nil {
		addr = *b.CustAddress
	}
	doc := Document{
		Version:    "1.1",
		TranDtls:   TranDtls{TaxSch: "GST", SupTyp: "B2B"},
		DocDtls:    DocDtls{Typ: "INV", No: deref(b.BillNo), Dt: ddmmyyyy(b.BillDate)},
		SellerDtls: seller(s),
		BuyerDtls: Party{Gstin: strings.TrimSpace(deref(b.CustGstin)), LglNm: b.CustName, Pos: b.PlaceOfSupply,
			Addr1: trim100(addr), Loc: "-", Pin: pin(addr), Stcd: gstinState(deref(b.CustGstin), b.PlaceOfSupply)},
		ValDtls: ValDtls{AssVal: b.TaxableTotal, CgstVal: b.Cgst, SgstVal: b.Sgst, IgstVal: b.Igst,
			RndOffAmt: b.RoundOff, TotInvVal: b.GrandTotal},
	}
	for _, l := range lines {
		gross := l.Qty.Mul(l.Rate).Round(2)
		doc.ItemList = append(doc.ItemList, DocItem{
			SlNo: itoa(len(doc.ItemList) + 1), PrdDesc: l.Description, IsServc: "N", HsnCd: l.HsnCode,
			Qty: l.Qty, Unit: uqc[l.UnitID], UnitPrice: l.Rate, TotAmt: gross,
			Discount: gross.Sub(l.TaxableValue), AssAmt: l.TaxableValue, GstRt: l.GstRate,
			IgstAmt: l.Igst, CgstAmt: l.Cgst, SgstAmt: l.Sgst, TotItemVal: l.LineTotal,
		})
	}
	for _, c := range charges {
		tax := c.Cgst.Add(c.Sgst).Add(c.Igst)
		doc.ItemList = append(doc.ItemList, DocItem{
			SlNo: itoa(len(doc.ItemList) + 1), PrdDesc: c.Description, IsServc: "Y", HsnCd: deref(c.SacCode),
			Qty: decimal.NewFromInt(1), Unit: "OTH", UnitPrice: c.Amount, TotAmt: c.Amount, AssAmt: c.Amount,
			GstRt: c.GstRate, IgstAmt: c.Igst, CgstAmt: c.Cgst, SgstAmt: c.Sgst, TotItemVal: c.Amount.Add(tax),
		})
	}
	return doc, nil
}

// CreditNoteDocument builds the payload for an approved credit note.
func CreditNoteDocument(ctx context.Context, q *dbq.Queries, s dbq.ShopSetting, cn dbq.GetCreditNoteRow) (Document, error) {
	b, err := q.GetBill(ctx, cn.BillID)
	if err != nil {
		return Document{}, err
	}
	lines, err := q.CreditNoteLines(ctx, cn.ID)
	if err != nil {
		return Document{}, err
	}
	ids := make([]int32, 0, len(lines))
	for _, l := range lines {
		ids = append(ids, l.UnitID)
	}
	rows, err := q.UnitUQCs(ctx, ids)
	if err != nil {
		return Document{}, err
	}
	uqc := map[int32]string{}
	for _, r := range rows {
		uqc[r.ID] = r.Uqc
	}
	addr := deref(b.CustAddress)
	doc := Document{
		Version:    "1.1",
		TranDtls:   TranDtls{TaxSch: "GST", SupTyp: "B2B"},
		DocDtls:    DocDtls{Typ: "CRN", No: deref(cn.CnNo), Dt: ddmmyyyy(cn.CnDate)},
		SellerDtls: seller(s),
		BuyerDtls: Party{Gstin: strings.TrimSpace(deref(b.CustGstin)), LglNm: b.CustName, Pos: b.PlaceOfSupply,
			Addr1: trim100(addr), Loc: "-", Pin: pin(addr), Stcd: gstinState(deref(b.CustGstin), b.PlaceOfSupply)},
		ValDtls: ValDtls{AssVal: cn.TaxableTotal, CgstVal: cn.Cgst, SgstVal: cn.Sgst, IgstVal: cn.Igst,
			TotInvVal: cn.GrandTotal},
		RefDtls: &RefDtls{PrecDocDtls: []DocDtls{{Typ: "INV", No: deref(b.BillNo), Dt: ddmmyyyy(b.BillDate)}}},
	}
	for _, l := range lines {
		tax := l.Cgst.Add(l.Sgst).Add(l.Igst)
		doc.ItemList = append(doc.ItemList, DocItem{
			SlNo: itoa(len(doc.ItemList) + 1), PrdDesc: l.Description, IsServc: "N", HsnCd: "",
			Qty: l.Qty, Unit: uqc[l.UnitID], UnitPrice: l.Rate, TotAmt: l.TaxableValue, AssAmt: l.TaxableValue,
			GstRt: l.GstRate, IgstAmt: l.Igst, CgstAmt: l.Cgst, SgstAmt: l.Sgst, TotItemVal: l.TaxableValue.Add(tax),
		})
	}
	return doc, nil
}

func uqcs(ctx context.Context, q *dbq.Queries, lines []dbq.BillLinesRow) (map[int32]string, error) {
	ids := make([]int32, 0, len(lines))
	for _, l := range lines {
		ids = append(ids, l.UnitID)
	}
	rows, err := q.UnitUQCs(ctx, ids)
	if err != nil {
		return nil, err
	}
	m := map[int32]string{}
	for _, r := range rows {
		m[r.ID] = r.Uqc
	}
	return m, nil
}

func gstinState(gstin, fallback string) string {
	if len(gstin) >= 2 {
		return gstin[:2]
	}
	return fallback
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
