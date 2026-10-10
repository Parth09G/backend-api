package billing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"billing-api/internal/api"
	"billing-api/internal/app"
	"billing-api/internal/db/dbq"
	"billing-api/internal/httpx"
	"billing-api/internal/inventory"
	"billing-api/internal/num"
	"billing-api/internal/settings"
)

type prepLine struct {
	itemID     int64
	locationID int16
	locName    string
	unitID     int32
	unit       string
	factor     decimal.Decimal
	baseQty    decimal.Decimal
	desc, hsn  string
	qty, rate  decimal.Decimal
	pct, amt   decimal.Decimal
	gst        decimal.Decimal
	out        LineOut
}

type prepCharge struct {
	kind   dbq.ChargeKind
	desc   string
	sac    *string
	amount decimal.Decimal
	gst    decimal.Decimal
	out    ChargeOut
}

// prepared is a validated, calculated bill ready to save.
type prepared struct {
	in         api.BillInput
	billType   dbq.BillKind
	date       time.Time
	partyID    *int64
	party      *dbq.PartiesByIDsRow
	custName   string
	gstin      *string
	address    *string
	mobile     *string
	site       *string
	pos        string
	interstate bool
	lines      []prepLine
	charges    []prepCharge
	calc       CalcResult
	paid       decimal.Decimal
	settings   dbq.ShopSetting
	copyOf     *dbq.Bill // Account copy of this Cash bill
}

func (p *prepared) affectsStock() bool  { return p.billType == dbq.BillKindCash && p.copyOf == nil }
func (p *prepared) postsToLedger() bool { return p.copyOf == nil }

// einvEligible: B2B bills (customer GSTIN) need an IRN; Account copies are
// duplicates of an e-invoiced Cash bill and do not (BR-04).
func (p *prepared) einvEligible() bool {
	return p.gstin != nil && p.settings.EinvoiceEnabled && p.copyOf == nil
}

func trimmed(s *string) *string {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	if t == "" {
		return nil
	}
	return &t
}

var chargeNames = map[string]string{"labour": "Labour charges", "transport": "Transport charges", "other": "Other charges"}

// prepare validates a bill request against the catalogue, parties and
// settings, and runs the calculator. calcOnly relaxes save-only rules.
func prepare(ctx context.Context, a *app.App, q *dbq.Queries, in api.BillInput, copyOf *dbq.Bill, calcOnly bool) (*prepared, error) {
	s, err := settings.Load(ctx, q)
	if err != nil {
		return nil, err
	}
	p := &prepared{in: in, billType: dbq.BillKind(in.BillType), date: a.DateOr(httpx.ParseDate(in.BillDate)),
		pos: in.PlaceOfSupply, settings: s, copyOf: copyOf}
	p.interstate = p.pos != s.StateCode

	// Customer (FR-BL-01, BR-09).
	c := in.Customer
	switch {
	case c.PartyID != nil:
		rows, err := q.PartiesByIDs(ctx, []int64{*c.PartyID})
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 || !rows[0].IsActive {
			return nil, httpx.Validation("customer.party_id", "unknown or inactive party")
		}
		if rows[0].Kind == dbq.PartyKindSupplier {
			return nil, httpx.Validation("customer.party_id", "party is a dealer, not a customer")
		}
		p.party, p.partyID = &rows[0], c.PartyID
		p.custName = rows[0].Name
		p.gstin = firstNonNil(trimmed(c.GSTIN), trimmed(rows[0].Gstin))
		p.address = firstNonNil(trimmed(c.BillingAddress), rows[0].BillingAddress)
		p.mobile = firstNonNil(trimmed(c.Mobile), rows[0].Mobile)
	case p.billType == dbq.BillKindAccount:
		return nil, httpx.Validation("customer.party_id", "Account bills need a named customer")
	default:
		name := trimmed(c.WalkInName)
		if name == nil {
			return nil, httpx.Validation("customer", "give party_id, or walk_in_name for a walk-in Cash sale")
		}
		p.custName = *name
		p.gstin, p.address, p.mobile = trimmed(c.GSTIN), trimmed(c.BillingAddress), trimmed(c.Mobile)
	}
	if p.gstin != nil {
		g := strings.ToUpper(*p.gstin)
		p.gstin = &g
	}
	p.site = trimmed(c.SiteAddress)

	// Lines.
	ids := make([]int64, len(in.Lines))
	for i, l := range in.Lines {
		ids[i] = l.ItemID
	}
	items, err := q.BillingItems(ctx, ids)
	if err != nil {
		return nil, err
	}
	byItem := map[int64]dbq.BillingItemsRow{}
	for _, it := range items {
		byItem[it.ID] = it
	}
	units, err := inventory.UnitFactors(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	locs, err := q.ActiveLocations(ctx)
	if err != nil {
		return nil, err
	}
	locNames := map[int16]string{}
	for _, l := range locs {
		locNames[l.ID] = l.Name
	}
	var defLoc *dbq.Location
	if l, err := q.DefaultBillingLocation(ctx); err == nil {
		defLoc = &l
	}
	calcIn := CalcInput{Interstate: p.interstate, InclusiveGST: s.PricesIncludeGst}
	for i, l := range in.Lines {
		f := fmt.Sprintf("lines[%d]", i)
		it, ok := byItem[l.ItemID]
		if !ok {
			return nil, httpx.Validation(f+".item_id", "unknown item")
		}
		if !it.IsActive {
			return nil, httpx.Validation(f+".item_id", "item is inactive")
		}
		var loc int16
		switch {
		case l.LocationID != nil:
			loc = int16(*l.LocationID)
		case defLoc != nil:
			loc = defLoc.ID
		default:
			return nil, httpx.Validation(f+".location_id", "is required (no default billing location is set)")
		}
		if _, ok := locNames[loc]; !ok {
			return nil, httpx.Validation(f+".location_id", "unknown or inactive location")
		}
		qty := num.Parse(l.Qty)
		base, factor, sym, err := inventory.BaseQty(units, f, l.ItemID, l.UnitID, qty)
		if err != nil {
			return nil, err
		}
		pl := prepLine{itemID: it.ID, locationID: loc, locName: locNames[loc], unitID: int32(l.UnitID), unit: sym,
			factor: factor, baseQty: base, desc: it.Name, hsn: it.HsnCode, qty: qty, rate: num.Parse(l.Rate),
			gst: it.GstRate}
		if d := trimmed(l.Description); d != nil {
			pl.desc = *d
		}
		if l.DiscountPct != nil {
			pl.pct = num.Parse(*l.DiscountPct)
		}
		if l.DiscountAmt != nil {
			pl.amt = num.Parse(*l.DiscountAmt)
		}
		p.lines = append(p.lines, pl)
		calcIn.Lines = append(calcIn.Lines, LineIn{Qty: qty, Rate: pl.rate, DiscPct: pl.pct, DiscAmt: pl.amt, GSTRate: pl.gst})
	}
	for _, ch := range in.Charges {
		pc := prepCharge{kind: dbq.ChargeKind(ch.Kind), desc: chargeNames[ch.Kind], sac: trimmed(ch.SACCode),
			amount: num.Parse(ch.Amount), gst: s.DefaultChargeGst}
		if d := trimmed(ch.Description); d != nil {
			pc.desc = *d
		}
		if ch.GSTRate != nil {
			pc.gst = num.Parse(*ch.GSTRate)
		}
		p.charges = append(p.charges, pc)
		calcIn.Charges = append(calcIn.Charges, ChargeIn{Amount: pc.amount, GSTRate: pc.gst})
	}
	if in.BillDiscount != nil {
		calcIn.BillDiscount = num.Parse(*in.BillDiscount)
	}
	res, err := Calculate(calcIn)
	var ce *CalcError
	if errors.As(err, &ce) {
		return nil, httpx.Validation(ce.Field, ce.Message)
	}
	if err != nil {
		return nil, err
	}
	p.calc = res
	for i := range p.lines {
		p.lines[i].out = res.Lines[i]
	}
	for i := range p.charges {
		p.charges[i].out = res.Charges[i]
	}
	if in.ExpectedTotal != nil && !num.Parse(*in.ExpectedTotal).Equal(res.GrandTotal) {
		return nil, httpx.Validation("expected_total", "the server's grand total is "+res.GrandTotal.StringFixed(2)+"; recalculate and try again")
	}

	// Payments taken on the bill screen (FR-BL-07, FR-AB-06, BR-09).
	for i, pay := range in.Payments {
		if !calcOnly && in.Status == "draft" {
			return nil, httpx.Validation("payments", "payments are recorded when the bill is saved as active")
		}
		if copyOf != nil {
			return nil, httpx.Validation("payments", "an Account copy does not take payments; record them on the Cash bill")
		}
		if p.billType == dbq.BillKindAccount && pay.Mode == "cash" {
			return nil, httpx.Validation(fmt.Sprintf("payments[%d].mode", i), "Account bills are paid by UPI, cheque or bank transfer")
		}
		p.paid = p.paid.Add(num.Parse(pay.Amount))
	}
	if p.paid.GreaterThan(res.GrandTotal) {
		return nil, httpx.Validation("payments", "payments add up to more than the grand total "+res.GrandTotal.StringFixed(2))
	}
	if !calcOnly && in.Status == "active" && p.partyID == nil && !p.paid.Equal(res.GrandTotal) && copyOf == nil {
		return nil, httpx.Validation("payments", "a walk-in customer must pay the full amount; choose a named customer for credit")
	}
	return p, nil
}

func firstNonNil(a, b *string) *string {
	if a != nil {
		return a
	}
	if b != nil && strings.TrimSpace(*b) != "" {
		t := strings.TrimSpace(*b)
		return &t
	}
	return nil
}

// creditProblem checks the customer's credit limit (FR-BL-08). The party's
// outstanding must not already include this bill.
func creditProblem(ctx context.Context, q *dbq.Queries, p *prepared) (*api.CreditWarning, error) {
	if p.party == nil || p.party.CreditLimit == nil || !p.postsToLedger() {
		return nil, nil
	}
	outstanding, err := q.PartyBalance(ctx, *p.partyID)
	if err != nil {
		return nil, err
	}
	this := p.calc.GrandTotal.Sub(p.paid)
	limit := *p.party.CreditLimit
	if outstanding.Add(this).LessThanOrEqual(limit) {
		return nil, nil
	}
	return &api.CreditWarning{Title: "Credit limit exceeded", Status: http.StatusUnprocessableEntity,
		Code: httpx.CodeCreditLimit, Detail: "outstanding plus this bill is more than the customer's credit limit",
		CreditLimit: api.M(limit), Outstanding: api.M(outstanding), ThisBill: api.M(this)}, nil
}

func creditError(w *api.CreditWarning) error {
	return &httpx.Problem{Status: http.StatusUnprocessableEntity, Code: httpx.CodeCreditLimit, Detail: w.Detail,
		Extra: map[string]any{"credit_limit": w.CreditLimit, "outstanding": w.Outstanding, "this_bill": w.ThisBill}}
}

// needs lists the stock the bill takes per line (Cash bills only).
func (p *prepared) needs() []inventory.Need {
	if !p.affectsStock() {
		return nil
	}
	out := make([]inventory.Need, len(p.lines))
	for i, l := range p.lines {
		out[i] = inventory.Need{LineNo: i + 1, ItemID: l.itemID, LocationID: l.locationID, Qty: l.baseQty}
	}
	return out
}

// apiLines renders prepared lines (calculate endpoint).
func (p *prepared) apiLines() []api.BillLine {
	out := make([]api.BillLine, len(p.lines))
	for i, l := range p.lines {
		loc := int64(l.locationID)
		name := l.locName
		out[i] = api.BillLine{LineNo: int32(i + 1), ItemID: l.itemID, LocationID: &loc, LocationName: &name,
			Description: l.desc, HSNCode: l.hsn, Qty: api.Q(l.qty), UnitID: int64(l.unitID), Unit: l.unit,
			BaseQty: api.Q(l.baseQty), Rate: api.M(l.rate), DiscountPct: num.Trim(l.pct), DiscountAmt: api.M(l.amt),
			TaxableValue: api.M(l.out.Taxable), GSTRate: num.Trim(l.gst), CGST: api.M(l.out.CGST), SGST: api.M(l.out.SGST),
			IGST: api.M(l.out.IGST), LineTotal: api.M(l.out.Total)}
	}
	return out
}

func (p *prepared) apiCharges() []api.BillCharge {
	out := make([]api.BillCharge, len(p.charges))
	for i, c := range p.charges {
		out[i] = api.BillCharge{Kind: string(c.kind), Description: c.desc, SACCode: c.sac, Amount: api.M(c.amount),
			GSTRate: num.Trim(c.gst), CGST: api.M(c.out.CGST), SGST: api.M(c.out.SGST), IGST: api.M(c.out.IGST)}
	}
	return out
}

func totals(c CalcResult, paid decimal.Decimal) api.BillTotals {
	t := api.BillTotals{ItemsTaxable: api.M(c.ItemsTaxable), ChargesTaxable: api.M(c.ChargesTaxable),
		BillDiscount: api.M(c.BillDiscount), TaxableTotal: api.M(c.TaxableTotal), CGST: api.M(c.CGST), SGST: api.M(c.SGST),
		IGST: api.M(c.IGST), RoundOff: api.M(c.RoundOff), GrandTotal: api.M(c.GrandTotal),
		AmountInWords: num.Words(c.GrandTotal), AmountPaid: api.M(paid), BalanceDue: api.M(c.GrandTotal.Sub(paid)),
		TaxByRate: []api.TaxSplit{}}
	for _, s := range c.TaxByRate {
		t.TaxByRate = append(t.TaxByRate, api.TaxSplit{GSTRate: num.Trim(s.Rate), Taxable: api.M(s.Taxable),
			CGST: api.M(s.CGST), SGST: api.M(s.SGST), IGST: api.M(s.IGST)})
	}
	return t
}
