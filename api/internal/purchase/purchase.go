// Package purchase handles dealer purchases into a location, purchase
// cancellation and purchase returns (FR-PU-01..03).
package purchase

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"billing-api/internal/api"
	"billing-api/internal/app"
	"billing-api/internal/audit"
	"billing-api/internal/auth"
	"billing-api/internal/db/dbq"
	"billing-api/internal/httpx"
	"billing-api/internal/idem"
	"billing-api/internal/inventory"
	"billing-api/internal/ledger"
	"billing-api/internal/num"
	"billing-api/internal/settings"
)

type handler struct{ a *app.App }

func Routes(a *app.App) func(chi.Router) {
	h := handler{a}
	return func(r chi.Router) {
		r.Get("/", httpx.H(h.list))
		r.Post("/", httpx.H(h.create))
		r.Get("/{id}", httpx.H(h.get))
		r.With(auth.SuperAdmin).Post("/{id}/cancel", httpx.H(h.cancel))
	}
}

func ReturnRoutes(a *app.App) func(chi.Router) {
	h := handler{a}
	return func(r chi.Router) {
		r.Post("/", httpx.H(h.createReturn))
	}
}

var hundred = decimal.NewFromInt(100)

// Load renders a purchase.
func Load(ctx context.Context, q *dbq.Queries, id int64) (api.Purchase, error) {
	pu, err := q.GetPurchase(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.Purchase{}, httpx.NotFound("purchase")
	}
	if err != nil {
		return api.Purchase{}, err
	}
	lines, err := q.PurchaseLines(ctx, id)
	if err != nil {
		return api.Purchase{}, err
	}
	paid, err := q.PurchasePaid(ctx, id)
	if err != nil {
		return api.Purchase{}, err
	}
	out := api.Purchase{ID: pu.ID, DocNo: pu.DocNo, SupplierID: pu.SupplierID, SupplierName: pu.SupplierName,
		SupplierInvoiceNo: pu.SupplierInvoiceNo, SupplierInvoiceDt: api.Date(pu.SupplierInvoiceDt),
		ReceivedDate: api.Date(pu.ReceivedDate), LocationID: int64(pu.LocationID), LocationName: pu.LocationName,
		Status: string(pu.Status), Lines: []api.PurchaseLine{}}
	byRate := map[string]*api.TaxSplit{}
	for _, l := range lines {
		out.Lines = append(out.Lines, api.PurchaseLine{ID: l.ID, ItemID: l.ItemID, ItemName: l.ItemName,
			LocationID: int64(l.LocationID), Qty: api.Q(l.Qty), Unit: l.UnitSymbol, BaseQty: api.Q(num.Z(l.BaseQty)),
			Rate: api.R(l.Rate), TaxableValue: api.M(l.TaxableValue), GSTRate: num.Trim(l.GstRate),
			LandedUnitCost: api.R(num.Z(l.LandedUnitCost))})
		k := num.Trim(l.GstRate)
		t := byRate[k]
		if t == nil {
			t = &api.TaxSplit{GSTRate: k}
			byRate[k] = t
		}
		t.Taxable = api.M(t.Taxable.D().Add(l.TaxableValue))
		t.CGST, t.SGST, t.IGST = api.M(t.CGST.D().Add(l.Cgst)), api.M(t.SGST.D().Add(l.Sgst)), api.M(t.IGST.D().Add(l.Igst))
	}
	charges := pu.Freight.Add(pu.OtherCharges)
	out.Totals = api.BillTotals{ItemsTaxable: api.M(pu.TaxableTotal), ChargesTaxable: api.M(charges),
		TaxableTotal: api.M(pu.TaxableTotal.Add(charges)), CGST: api.M(pu.Cgst), SGST: api.M(pu.Sgst), IGST: api.M(pu.Igst),
		RoundOff: api.M(pu.RoundOff), GrandTotal: api.M(pu.GrandTotal), AmountInWords: num.Words(pu.GrandTotal),
		AmountPaid: api.M(paid), BalanceDue: api.M(pu.GrandTotal.Sub(paid)), TaxByRate: []api.TaxSplit{}}
	for _, t := range byRate {
		out.Totals.TaxByRate = append(out.Totals.TaxByRate, *t)
	}
	out.BalancePayable = api.M(pu.GrandTotal.Sub(paid))
	if pu.Status == dbq.DocStatusCancelled {
		out.BalancePayable = api.M(decimal.Zero)
	}
	return out, nil
}

func (h handler) list(w http.ResponseWriter, r *http.Request) error {
	page, err := httpx.ParsePage(r)
	if err != nil {
		return err
	}
	party, err := httpx.QInt64(r, "party_id")
	if err != nil {
		return err
	}
	loc, err := httpx.QInt64(r, "location_id")
	if err != nil {
		return err
	}
	from, to, err := httpx.Range(r)
	if err != nil {
		return err
	}
	sort, desc, err := httpx.Sort(r, "-received_date", "received_date", "doc_no", "grand_total")
	if err != nil {
		return err
	}
	p := dbq.ListPurchaseIDsParams{SupplierID: party, FromDate: from, ToDate: to, Q: httpx.QString(r, "q"),
		Sort: sort, Descending: desc, Lim: page.Limit(), Off: page.Offset()}
	if loc != nil {
		l := int16(*loc)
		p.LocationID = &l
	}
	ctx := r.Context()
	rows, err := h.a.Q.ListPurchaseIDs(ctx, p)
	if err != nil {
		return err
	}
	out := make([]api.Purchase, 0, len(rows))
	var total int64
	for _, row := range rows {
		total = row.Total
		pu, err := Load(ctx, h.a.Q, row.ID)
		if err != nil {
			return err
		}
		out = append(out, pu)
	}
	httpx.JSON(w, http.StatusOK, httpx.NewList(out, page, total))
	return nil
}

func (h handler) get(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	pu, err := Load(r.Context(), h.a.Q, id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, pu)
	return nil
}

// spread divides amount over weights proportionally (remainder on the largest).
func spread(amount decimal.Decimal, weights []decimal.Decimal) []decimal.Decimal {
	out := make([]decimal.Decimal, len(weights))
	total, largest := decimal.Zero, 0
	for i, w := range weights {
		total = total.Add(w)
		if w.GreaterThan(weights[largest]) {
			largest = i
		}
	}
	if amount.IsZero() || !total.IsPositive() {
		return out
	}
	given := decimal.Zero
	for i, w := range weights {
		if i != largest {
			out[i] = amount.Mul(w).Div(total).Round(2)
			given = given.Add(out[i])
		}
	}
	out[largest] = amount.Sub(given)
	return out
}

// create records a purchase (FR-PU-01/02): stock in at each line's location
// at landed cost (taxable value + apportioned freight and other charges,
// GST excluded as input credit), weighted average cost updated by
// fn_stock_in, dealer credited for the grand total.
func (h handler) create(w http.ResponseWriter, r *http.Request) error {
	body, err := httpx.ReadBody(r)
	if err != nil {
		return err
	}
	var in api.PurchaseInput
	if err := httpx.DecodeBytes(body, &in); err != nil {
		return err
	}
	return idem.Run(w, r, h.a, body, func(ctx context.Context, _ pgx.Tx, q *dbq.Queries) (idem.Result, error) {
		s, err := settings.Load(ctx, q)
		if err != nil {
			return idem.Result{}, err
		}
		parties, err := q.PartiesByIDs(ctx, []int64{in.SupplierID})
		if err != nil {
			return idem.Result{}, err
		}
		if len(parties) == 0 || !parties[0].IsActive || parties[0].Kind == dbq.PartyKindCustomer {
			return idem.Result{}, httpx.Validation("supplier_id", "unknown or inactive dealer")
		}
		supplier := parties[0]
		locs, err := q.ActiveLocations(ctx)
		if err != nil {
			return idem.Result{}, err
		}
		active := map[int16]bool{}
		for _, l := range locs {
			active[l.ID] = true
		}
		if !active[int16(in.LocationID)] {
			return idem.Result{}, httpx.Validation("location_id", "unknown or inactive location")
		}
		interstate := supplier.StateCode != nil && strings.TrimSpace(*supplier.StateCode) != "" && *supplier.StateCode != s.StateCode
		if in.IsInterstate != nil {
			interstate = *in.IsInterstate
		}
		itemIDs := make([]int64, len(in.Lines))
		for i, l := range in.Lines {
			itemIDs[i] = l.ItemID
		}
		items, err := q.BillingItems(ctx, itemIDs)
		if err != nil {
			return idem.Result{}, err
		}
		byItem := map[int64]dbq.BillingItemsRow{}
		for _, it := range items {
			byItem[it.ID] = it
		}
		units, err := inventory.UnitFactors(ctx, q, itemIDs)
		if err != nil {
			return idem.Result{}, err
		}

		type line struct {
			p   dbq.InsertPurchaseLineParams
			loc int16
		}
		lines := make([]line, len(in.Lines))
		weights := make([]decimal.Decimal, len(in.Lines))
		taxable, cgst, sgst, igst := decimal.Zero, decimal.Zero, decimal.Zero, decimal.Zero
		for i, l := range in.Lines {
			f := fmt.Sprintf("lines[%d]", i)
			it, ok := byItem[l.ItemID]
			if !ok || !it.IsActive {
				return idem.Result{}, httpx.Validation(f+".item_id", "unknown or inactive item")
			}
			loc := int16(in.LocationID)
			if l.LocationID != nil {
				loc = int16(*l.LocationID)
			}
			if !active[loc] {
				return idem.Result{}, httpx.Validation(f+".location_id", "unknown or inactive location")
			}
			qty := num.Parse(l.Qty)
			_, factor, _, err := inventory.BaseQty(units, f, l.ItemID, l.UnitID, qty)
			if err != nil {
				return idem.Result{}, err
			}
			rate := num.Parse(l.Rate)
			gross := qty.Mul(rate).Round(2)
			disc := decimal.Zero
			if l.DiscountAmt != nil {
				disc = num.Parse(*l.DiscountAmt)
			}
			if disc.GreaterThan(gross) {
				return idem.Result{}, httpx.Validation(f+".discount_amt", "is more than the line amount")
			}
			gst := it.GstRate
			if l.GSTRate != nil {
				gst = num.Parse(*l.GSTRate)
			}
			tv := gross.Sub(disc)
			c, sg, ig := decimal.Zero, decimal.Zero, decimal.Zero
			if interstate {
				ig = tv.Mul(gst).Div(hundred).Round(2)
			} else {
				c = tv.Mul(gst).Div(hundred).Div(decimal.NewFromInt(2)).Round(2)
				sg = c
			}
			taxable, cgst, sgst, igst = taxable.Add(tv), cgst.Add(c), sgst.Add(sg), igst.Add(ig)
			weights[i] = tv
			lines[i] = line{p: dbq.InsertPurchaseLineParams{ItemID: l.ItemID, LocationID: loc, Qty: qty, UnitID: int32(l.UnitID),
				FactorToBase: factor, Rate: rate, DiscountAmt: disc, TaxableValue: tv, GstRate: gst, Cgst: c, Sgst: sg, Igst: ig},
				loc: loc}
		}
		freight, other := decimal.Zero, decimal.Zero
		if in.Freight != nil {
			freight = num.Parse(*in.Freight)
		}
		if in.OtherCharges != nil {
			other = num.Parse(*in.OtherCharges)
		}
		shares := spread(freight.Add(other), weights)
		for i := range lines {
			base := lines[i].p.Qty.Mul(lines[i].p.FactorToBase).Round(3)
			landed := lines[i].p.TaxableValue.Add(shares[i]).Div(base).Round(4)
			lines[i].p.LandedUnitCost = &landed
		}
		exact := taxable.Add(freight).Add(other).Add(cgst).Add(sgst).Add(igst)
		grand := exact.Round(0)

		date := h.a.DateOr(httpx.ParseDate(in.ReceivedDate))
		docNo, err := q.NextDocNo(ctx, dbq.NextDocNoParams{DocType: dbq.DocKindPurchase, DocDate: date})
		if err != nil {
			return idem.Result{}, err
		}
		fy, err := q.FY(ctx, date)
		if err != nil {
			return idem.Result{}, err
		}
		user := auth.UserID(ctx)
		invDate := *httpx.ParseDate(&in.SupplierInvoiceDt)
		id, err := q.InsertPurchase(ctx, dbq.InsertPurchaseParams{DocNo: docNo, Fy: fy, SupplierID: supplier.ID,
			SupplierInvoiceNo: strings.TrimSpace(in.SupplierInvoiceNo), SupplierInvoiceDt: invDate, ReceivedDate: date,
			LocationID: int16(in.LocationID), IsInterstate: interstate, TaxableTotal: taxable, Freight: freight,
			OtherCharges: other, Cgst: cgst, Sgst: sgst, Igst: igst, RoundOff: grand.Sub(exact), GrandTotal: grand,
			Notes: in.Notes, CreatedBy: *user})
		if err != nil {
			return idem.Result{}, err
		}
		ref := dbq.DocKindPurchase
		for _, l := range lines {
			l.p.PurchaseID = id
			lineID, err := q.InsertPurchaseLine(ctx, l.p)
			if err != nil {
				return idem.Result{}, err
			}
			base := l.p.Qty.Mul(l.p.FactorToBase).Round(3)
			if err := q.StockIn(ctx, dbq.StockInParams{ItemID: l.p.ItemID, LocationID: l.loc, Qty: base,
				Cost: l.p.LandedUnitCost, Kind: dbq.MovementKindPurchase, RefType: &ref, RefID: &id, RefLine: &lineID,
				UserID: user}); err != nil {
				return idem.Result{}, err
			}
		}
		narr := "Purchase " + docNo + " (inv " + strings.TrimSpace(in.SupplierInvoiceNo) + ")"
		if err := q.InsertLedger(ctx, dbq.InsertLedgerParams{PartyID: supplier.ID, EntryDate: date, RefType: ref, RefID: id,
			Credit: grand, Narration: &narr}); err != nil {
			return idem.Result{}, err
		}
		if in.Payment != nil {
			if err := httpx.Validate(in.Payment); err != nil {
				return idem.Result{}, err
			}
			amt := num.Parse(in.Payment.Amount)
			if amt.GreaterThan(grand) {
				return idem.Result{}, httpx.Validation("payment.amount", "is more than the purchase total")
			}
			np := ledger.FromSplit(*in.Payment, dbq.PayDirectionPaid, &supplier.ID, date)
			pid, _, err := ledger.Record(ctx, q, np)
			if err != nil {
				return idem.Result{}, err
			}
			if err := ledger.Allocate(ctx, q, pid, dbq.DocKindPurchase, id, amt); err != nil {
				return idem.Result{}, err
			}
		}
		out, err := Load(ctx, q, id)
		if err != nil {
			return idem.Result{}, err
		}
		if err := audit.Log(ctx, q, "create", "purchase", audit.ID(id), nil, out); err != nil {
			return idem.Result{}, err
		}
		return idem.Result{Status: http.StatusCreated, Body: out, Location: fmt.Sprintf("/api/v1/purchases/%d", id)}, nil
	})
}

// cancel reverses a purchase while its stock is still on hand; otherwise
// a purchase return is needed (422 INSUFFICIENT_STOCK).
func (h handler) cancel(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in api.ReasonInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	var out api.Purchase
	err = h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		pu, err := q.LockPurchase(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.NotFound("purchase")
		}
		if err != nil {
			return err
		}
		if pu.Status != dbq.DocStatusActive {
			return httpx.InvalidState("the purchase is already cancelled")
		}
		if has, err := q.PurchaseHasReturns(ctx, id); err != nil {
			return err
		} else if has {
			return httpx.InvalidState("the purchase has returns; return the remaining goods instead of cancelling")
		}
		before, err := Load(ctx, q, id)
		if err != nil {
			return err
		}
		lines, err := q.PurchaseLines(ctx, id)
		if err != nil {
			return err
		}
		needs := make([]inventory.Need, len(lines))
		for i, l := range lines {
			needs[i] = inventory.Need{LineNo: i + 1, ItemID: l.ItemID, LocationID: l.LocationID, Qty: num.Z(l.BaseQty)}
		}
		short, err := inventory.CheckShortages(ctx, q, needs, true)
		if err != nil {
			return err
		}
		if len(short) > 0 {
			return inventory.ShortageProblem(short, false)
		}
		user := auth.UserID(ctx)
		for _, l := range lines {
			lineID := l.ID
			if _, err := q.StockOut(ctx, dbq.StockOutParams{ItemID: l.ItemID, LocationID: l.LocationID, Qty: num.Z(l.BaseQty),
				Kind: dbq.MovementKindPurchaseReturn, RefType: dbq.DocKindPurchase, RefID: id, RefLine: &lineID, UserID: *user}); err != nil {
				return err
			}
			if err := q.RemoveCost(ctx, dbq.RemoveCostParams{ItemID: l.ItemID, Qty: num.Z(l.BaseQty), Cost: num.Z(l.LandedUnitCost)}); err != nil {
				return err
			}
		}
		if err := q.ReverseLedger(ctx, dbq.ReverseLedgerParams{EntryDate: h.a.Today(), RefType: dbq.DocKindPurchase, RefID: id}); err != nil {
			return err
		}
		if err := q.DetachPurchaseAllocations(ctx, id); err != nil {
			return err
		}
		if err := q.CancelPurchaseRow(ctx, dbq.CancelPurchaseRowParams{ID: id, Reason: &in.Reason, UserID: user}); err != nil {
			return err
		}
		if out, err = Load(ctx, q, id); err != nil {
			return err
		}
		return audit.Log(ctx, q, "cancel", "purchase", audit.ID(id), before, map[string]any{"purchase": out, "reason": in.Reason})
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, out)
	return nil
}

// createReturn sends goods back to the dealer (FR-PU-03): stock out at the
// line's location, average cost corrected, dealer debited.
func (h handler) createReturn(w http.ResponseWriter, r *http.Request) error {
	body, err := httpx.ReadBody(r)
	if err != nil {
		return err
	}
	var in api.PurchaseReturnInput
	if err := httpx.DecodeBytes(body, &in); err != nil {
		return err
	}
	err = idem.Run(w, r, h.a, body, func(ctx context.Context, _ pgx.Tx, q *dbq.Queries) (idem.Result, error) {
		pu, err := q.LockPurchase(ctx, in.PurchaseID)
		if errors.Is(err, pgx.ErrNoRows) {
			return idem.Result{}, httpx.Validation("purchase_id", "unknown purchase")
		}
		if err != nil {
			return idem.Result{}, err
		}
		if pu.Status != dbq.DocStatusActive {
			return idem.Result{}, httpx.InvalidState("the purchase is cancelled")
		}
		plines, err := q.PurchaseLines(ctx, pu.ID)
		if err != nil {
			return idem.Result{}, err
		}
		byLine := map[int64]dbq.PurchaseLinesRow{}
		ids := make([]int64, 0, len(plines))
		itemIDs := make([]int64, 0, len(plines))
		for _, l := range plines {
			byLine[l.ID] = l
			ids = append(ids, l.ID)
			itemIDs = append(itemIDs, l.ItemID)
		}
		done := map[int64]decimal.Decimal{}
		ret, err := q.PurchaseReturnedQty(ctx, ids)
		if err != nil {
			return idem.Result{}, err
		}
		for _, rq := range ret {
			done[rq.PurchaseLineID] = rq.Qty
		}
		units, err := inventory.UnitFactors(ctx, q, itemIDs)
		if err != nil {
			return idem.Result{}, err
		}
		type rline struct {
			pl     dbq.PurchaseLinesRow
			base   decimal.Decimal
			amount decimal.Decimal
		}
		var rls []rline
		var needs []inventory.Need
		total := decimal.Zero
		for i, l := range in.Lines {
			f := fmt.Sprintf("lines[%d]", i)
			pl, ok := byLine[l.PurchaseLineID]
			if !ok {
				return idem.Result{}, httpx.Validation(f+".purchase_line_id", "not a line of this purchase")
			}
			base, _, _, err := inventory.BaseQty(units, f, pl.ItemID, l.UnitID, num.Parse(l.Qty))
			if err != nil {
				return idem.Result{}, err
			}
			bought := num.Z(pl.BaseQty)
			if done[pl.ID].Add(base).GreaterThan(bought) {
				return idem.Result{}, httpx.Validation(f+".qty", "more than the quantity still returnable ("+
					bought.Sub(done[pl.ID]).StringFixed(3)+" base units)")
			}
			done[pl.ID] = done[pl.ID].Add(base)
			value := pl.TaxableValue.Add(pl.Cgst).Add(pl.Sgst).Add(pl.Igst)
			amount := value.Mul(base).Div(bought).Round(2)
			total = total.Add(amount)
			rls = append(rls, rline{pl, base, amount})
			needs = append(needs, inventory.Need{LineNo: i + 1, ItemID: pl.ItemID, LocationID: pl.LocationID, Qty: base})
		}
		short, err := inventory.CheckShortages(ctx, q, needs, true)
		if err != nil {
			return idem.Result{}, err
		}
		if len(short) > 0 {
			return idem.Result{}, &inventory.ShortageError{Lines: short}
		}
		date := h.a.DateOr(httpx.ParseDate(in.ReturnDate))
		docNo, err := q.NextDocNo(ctx, dbq.NextDocNoParams{DocType: dbq.DocKindPurchaseReturn, DocDate: date})
		if err != nil {
			return idem.Result{}, err
		}
		fy, err := q.FY(ctx, date)
		if err != nil {
			return idem.Result{}, err
		}
		user := auth.UserID(ctx)
		id, err := q.InsertPurchaseReturn(ctx, dbq.InsertPurchaseReturnParams{DocNo: docNo, Fy: fy, PurchaseID: pu.ID,
			ReturnDate: date, Reason: strings.TrimSpace(in.Reason), GrandTotal: total, CreatedBy: *user})
		if err != nil {
			return idem.Result{}, err
		}
		for _, rl := range rls {
			if err := q.InsertPurchaseReturnLine(ctx, dbq.InsertPurchaseReturnLineParams{PurchaseReturnID: id,
				PurchaseLineID: rl.pl.ID, ItemID: rl.pl.ItemID, LocationID: rl.pl.LocationID, BaseQty: rl.base,
				Rate: num.Z(rl.pl.LandedUnitCost), Amount: rl.amount}); err != nil {
				return idem.Result{}, err
			}
			if _, err := q.StockOut(ctx, dbq.StockOutParams{ItemID: rl.pl.ItemID, LocationID: rl.pl.LocationID, Qty: rl.base,
				Kind: dbq.MovementKindPurchaseReturn, RefType: dbq.DocKindPurchaseReturn, RefID: id, UserID: *user}); err != nil {
				return idem.Result{}, err
			}
			if err := q.RemoveCost(ctx, dbq.RemoveCostParams{ItemID: rl.pl.ItemID, Qty: rl.base, Cost: num.Z(rl.pl.LandedUnitCost)}); err != nil {
				return idem.Result{}, err
			}
		}
		narr := "Purchase return " + docNo + " against " + pu.DocNo
		if err := q.InsertLedger(ctx, dbq.InsertLedgerParams{PartyID: pu.SupplierID, EntryDate: date,
			RefType: dbq.DocKindPurchaseReturn, RefID: id, Debit: total, Narration: &narr}); err != nil {
			return idem.Result{}, err
		}
		out := api.DocCreated{ID: id, DocNo: docNo}
		if err := audit.Log(ctx, q, "create", "purchase_return", audit.ID(id), nil, map[string]any{"return": out, "lines": in.Lines}); err != nil {
			return idem.Result{}, err
		}
		return idem.Result{Status: http.StatusCreated, Body: out}, nil
	})
	var se *inventory.ShortageError
	if errors.As(err, &se) {
		return inventory.ShortageProblem(se.Lines, false)
	}
	return err
}
