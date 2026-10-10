package billing

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"billing-api/internal/api"
	"billing-api/internal/app"
	"billing-api/internal/auth"
	"billing-api/internal/db/dbq"
	"billing-api/internal/einvoice"
	"billing-api/internal/httpx"
	"billing-api/internal/num"
)

func refKind(t dbq.BillKind) dbq.DocKind {
	if t == dbq.BillKindAccount {
		return dbq.DocKindAccountBill
	}
	return dbq.DocKindCashBill
}

func docKindFor(t dbq.BillKind) dbq.DocKind { return refKind(t) }

// windowClosed reports whether the IRN is older than the edit window
// (24 hours by default) — then edits and cancels need a credit note (BR-08).
func windowClosed(b dbq.GetBillRow, s *dbq.ShopSetting, now time.Time) bool {
	if b.EinvStatus != dbq.EinvStateGenerated || b.AckDate == nil {
		return false
	}
	hrs := int32(24)
	if s != nil && s.BillEditWindowHrs > 0 {
		hrs = s.BillEditWindowHrs
	}
	return now.After(b.AckDate.Add(time.Duration(hrs) * time.Hour))
}

func periodLocked(date time.Time, s *dbq.ShopSetting) bool {
	return s != nil && s.LockedUntil != nil && !date.After(*s.LockedUntil)
}

// LoadBill renders a bill with lines, charges, payments, e-invoice state and
// the actions this user may take (API design 2.2).
func LoadBill(ctx context.Context, a *app.App, q *dbq.Queries, id int64) (api.Bill, dbq.GetBillRow, error) {
	b, err := q.GetBill(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.Bill{}, b, httpx.NotFound("bill")
	}
	if err != nil {
		return api.Bill{}, b, err
	}
	lines, err := q.BillLines(ctx, id)
	if err != nil {
		return api.Bill{}, b, err
	}
	charges, err := q.BillCharges(ctx, id)
	if err != nil {
		return api.Bill{}, b, err
	}
	pays, err := q.BillPayments(ctx, dbq.BillPaymentsParams{RefType: refKind(b.BillType), BillID: id})
	if err != nil {
		return api.Bill{}, b, err
	}
	var s *dbq.ShopSetting
	if row, err := q.GetSettings(ctx); err == nil {
		s = &row
	}

	out := api.Bill{ID: b.ID, BillType: string(b.BillType), BillNo: b.BillNo, FY: trimmed(b.Fy), BillDate: api.Date(b.BillDate),
		Status: string(b.Status), Version: b.Version, PlaceOfSupply: b.PlaceOfSupply, IsInterstate: b.IsInterstate,
		VehicleNo: b.VehicleNo, AffectsStock: b.AffectsStock, PostsToLedger: b.PostsToLedger, Notes: b.Notes,
		CancelReason: b.CancelReason, CreatedByName: b.CreatedByName, CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt,
		Customer: api.BillCustomer{PartyID: b.PartyID, Name: b.CustName, GSTIN: trimmed(b.CustGstin), Address: b.CustAddress,
			Mobile: b.CustMobile, SiteAddress: b.SiteAddress},
		Lines: []api.BillLine{}, Charges: []api.BillCharge{}, Payments: []api.BillPayment{}}

	if b.SourceBillID != nil {
		if src, err := q.GetBill(ctx, *b.SourceBillID); err == nil {
			out.SourceBill = &api.BillRef{ID: src.ID, BillNo: deref(src.BillNo)}
		}
	}
	var copyRef *api.BillRef
	if c, err := q.ActiveAccountCopy(ctx, &b.ID); err == nil {
		copyRef = &api.BillRef{ID: c.ID, BillNo: c.BillNo}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return api.Bill{}, b, err
	}
	out.AccountCopy = copyRef

	byRate := map[string]*api.TaxSplit{}
	var rates []string
	addTax := func(rate decimal.Decimal, taxable, c, sg, ig decimal.Decimal) {
		k := num.Trim(rate)
		t := byRate[k]
		if t == nil {
			t = &api.TaxSplit{GSTRate: k}
			byRate[k] = t
			rates = append(rates, k)
		}
		t.Taxable = api.M(t.Taxable.D().Add(taxable))
		t.CGST, t.SGST, t.IGST = api.M(t.CGST.D().Add(c)), api.M(t.SGST.D().Add(sg)), api.M(t.IGST.D().Add(ig))
	}
	for _, l := range lines {
		loc := int64(l.LocationID)
		name := l.LocationName
		out.Lines = append(out.Lines, api.BillLine{ID: l.ID, LineNo: int32(l.LineNo), ItemID: l.ItemID, LocationID: &loc,
			LocationName: &name, Description: l.Description, HSNCode: l.HsnCode, Qty: api.Q(l.Qty), UnitID: int64(l.UnitID),
			Unit: l.UnitSymbol, BaseQty: api.Q(num.Z(l.BaseQty)), Rate: api.M(l.Rate), DiscountPct: num.Trim(l.DiscountPct),
			DiscountAmt: api.M(l.DiscountAmt), TaxableValue: api.M(l.TaxableValue), GSTRate: num.Trim(l.GstRate),
			CGST: api.M(l.Cgst), SGST: api.M(l.Sgst), IGST: api.M(l.Igst), LineTotal: api.M(l.LineTotal),
			UnitCost: api.RP(l.UnitCost)})
		addTax(l.GstRate, l.TaxableValue, l.Cgst, l.Sgst, l.Igst)
	}
	for _, c := range charges {
		out.Charges = append(out.Charges, api.BillCharge{ID: c.ID, Kind: string(c.Kind), Description: c.Description,
			SACCode: c.SacCode, Amount: api.M(c.Amount), GSTRate: num.Trim(c.GstRate), CGST: api.M(c.Cgst),
			SGST: api.M(c.Sgst), IGST: api.M(c.Igst)})
		addTax(c.GstRate, c.Amount, c.Cgst, c.Sgst, c.Igst)
	}
	for _, p := range pays {
		out.Payments = append(out.Payments, api.BillPayment{PaymentID: p.PaymentID, DocNo: p.DocNo, Mode: string(p.Mode),
			Amount: api.M(p.Amount), Date: api.Date(p.PayDate)})
	}
	sortRates(rates)
	out.Totals = api.BillTotals{ItemsTaxable: api.M(b.ItemsTaxable), ChargesTaxable: api.M(b.ChargesTaxable),
		BillDiscount: api.M(b.BillDiscount), TaxableTotal: api.M(b.TaxableTotal), CGST: api.M(b.Cgst), SGST: api.M(b.Sgst),
		IGST: api.M(b.Igst), RoundOff: api.M(b.RoundOff), GrandTotal: api.M(b.GrandTotal),
		AmountInWords: num.Words(b.GrandTotal), AmountPaid: api.M(b.AmountPaid), BalanceDue: api.M(num.Z(b.BalanceDue)),
		TaxByRate: []api.TaxSplit{}}
	for _, k := range rates {
		out.Totals.TaxByRate = append(out.Totals.TaxByRate, *byRate[k])
	}
	out.CostTotal = api.M(b.CostTotal)
	out.GrossProfit = api.M(b.ItemsTaxable.Sub(b.CostTotal))

	hrs := int32(24)
	if s != nil {
		hrs = s.BillEditWindowHrs
	}
	out.EInvoice = api.EInvoice{Status: string(b.EinvStatus), IRN: trimmed(b.Irn), AckNo: b.AckNo, AckDate: b.AckDate,
		SignedQR: b.SignedQr, EWBNo: b.EwbNo, EWBValidUntil: b.EwbValidUntil, LastError: b.EinvError}
	if b.EinvStatus == dbq.EinvStateGenerated {
		out.EInvoice.CancelDeadline = einvoice.CancelDeadline(b.AckDate, hrs)
	}

	hasReturns, err := q.BillHasReturns(ctx, b.ID)
	if err != nil {
		return api.Bill{}, b, err
	}
	out.AllowedActions = allowedActions(ctx, a, b, s, copyRef != nil, hasReturns)
	return out, b, nil
}

func sortRates(r []string) {
	for i := 1; i < len(r); i++ {
		for j := i; j > 0 && num.Parse(r[j]).LessThan(num.Parse(r[j-1])); j-- {
			r[j], r[j-1] = r[j-1], r[j]
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func allowedActions(ctx context.Context, a *app.App, b dbq.GetBillRow, s *dbq.ShopSetting, hasCopy, hasReturns bool) []string {
	u, _ := auth.FromContext(ctx)
	sa := u.IsSuperAdmin()
	acts := []string{}
	add := func(ok bool, act string) {
		if ok {
			acts = append(acts, act)
		}
	}
	switch b.Status {
	case dbq.DocStatusDraft:
		add(true, "edit")
	case dbq.DocStatusActive:
		closed := windowClosed(b, s, a.Now())
		locked := periodLocked(b.BillDate, s)
		isCopy := b.SourceBillID != nil
		einvOn := s != nil && s.EinvoiceEnabled
		b2b := trimmed(b.CustGstin) != nil
		add(sa && !closed && !locked && !hasReturns, "edit")
		add(sa && !closed && !hasCopy && !hasReturns, "cancel")
		add(b.BillType == dbq.BillKindCash && !hasCopy && b.PartyID != nil, "copy_to_account")
		add(b.PostsToLedger && b.PartyID != nil && num.Z(b.BalanceDue).IsPositive(), "record_payment")
		add(!isCopy, "create_return")
		add(!isCopy && b2b && einvOn && (b.EinvStatus == dbq.EinvStateFailed || b.EinvStatus == dbq.EinvStateNotRequired ||
			b.EinvStatus == dbq.EinvStatePending || b.EinvStatus == dbq.EinvStateCancelled), "generate_einvoice")
		add(sa && b.EinvStatus == dbq.EinvStateGenerated && !closed, "cancel_einvoice")
		add(b.EwbNo == nil && (b.EinvStatus == dbq.EinvStateGenerated || !b2b || !einvOn || isCopy), "generate_ewaybill")
		printable := !(b.EinvStatus == dbq.EinvStatePending || b.EinvStatus == dbq.EinvStateFailed)
		add(printable, "print")
		add(printable, "share")
	case dbq.DocStatusCancelled:
		acts = append(acts, "print", "share")
	}
	return acts
}
