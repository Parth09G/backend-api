// Package returns handles sales returns and credit notes (FR-RT): created by
// any user, approved by the Super Admin (auto-approved when the Super Admin
// creates it). Approval numbers the credit note, restores stock for Cash
// bills (restockable lines, at the original sale cost), credits the customer
// or records a refund, and queues the e-invoice when the original had one.
package returns

import (
	"context"
	"encoding/json"
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
	"billing-api/internal/docs"
	"billing-api/internal/einvoice"
	"billing-api/internal/httpx"
	"billing-api/internal/idem"
	"billing-api/internal/inventory"
	"billing-api/internal/ledger"
	"billing-api/internal/num"
)

type handler struct{ a *app.App }

func Routes(a *app.App) func(chi.Router) {
	h := handler{a}
	return func(r chi.Router) {
		r.Get("/", httpx.H(h.list))
		r.Post("/", httpx.H(h.create))
		r.Get("/{id}", httpx.H(h.get))
		r.With(auth.SuperAdmin).Post("/{id}/approve", httpx.H(h.approve))
		r.With(auth.SuperAdmin).Post("/{id}/reject", httpx.H(h.reject))
		r.Get("/{id}/pdf", httpx.H(h.pdf))
	}
}

var hundred = decimal.NewFromInt(100)

// Load renders a credit note.
func Load(ctx context.Context, q *dbq.Queries, id int64) (api.CreditNote, error) {
	cn, err := q.GetCreditNote(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.CreditNote{}, httpx.NotFound("return")
	}
	if err != nil {
		return api.CreditNote{}, err
	}
	lines, err := q.CreditNoteLines(ctx, id)
	if err != nil {
		return api.CreditNote{}, err
	}
	out := api.CreditNote{ID: cn.ID, CNNo: cn.CnNo, CNDate: api.Date(cn.CnDate), BillID: cn.BillID, BillNo: cn.BillNo,
		PartyName: cn.CustName, Status: string(cn.Status), Reason: cn.Reason, Settlement: deref(cn.Settlement),
		CreatedByName: cn.CreatedByName, ApprovedByName: cn.ApprovedByName, ApprovedAt: cn.ApprovedAt,
		Lines: []api.CreditNoteLine{},
		EInvoice: api.EInvoice{Status: string(cn.EinvStatus), IRN: cn.Irn, AckNo: cn.AckNo, AckDate: cn.AckDate,
			SignedQR: cn.SignedQr, LastError: cn.EinvError},
		Totals: api.BillTotals{ItemsTaxable: api.M(cn.TaxableTotal), TaxableTotal: api.M(cn.TaxableTotal),
			CGST: api.M(cn.Cgst), SGST: api.M(cn.Sgst), IGST: api.M(cn.Igst), GrandTotal: api.M(cn.GrandTotal),
			AmountInWords: num.Words(cn.GrandTotal), TaxByRate: []api.TaxSplit{}}}
	byRate := map[string]*api.TaxSplit{}
	for _, l := range lines {
		out.Lines = append(out.Lines, api.CreditNoteLine{BillLineID: l.BillLineID, ItemID: l.ItemID, Description: l.Description,
			Qty: api.Q(l.Qty), Unit: l.UnitSymbol, LocationID: int64(l.LocationID), Rate: api.M(l.Rate),
			TaxableValue: api.M(l.TaxableValue), Restock: l.Restock})
		k := num.Trim(l.GstRate)
		t := byRate[k]
		if t == nil {
			t = &api.TaxSplit{GSTRate: k}
			byRate[k] = t
		}
		t.Taxable = api.M(t.Taxable.D().Add(l.TaxableValue))
		t.CGST, t.SGST, t.IGST = api.M(t.CGST.D().Add(l.Cgst)), api.M(t.SGST.D().Add(l.Sgst)), api.M(t.IGST.D().Add(l.Igst))
	}
	for _, t := range byRate {
		out.Totals.TaxByRate = append(out.Totals.TaxByRate, *t)
	}
	return out, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (h handler) list(w http.ResponseWriter, r *http.Request) error {
	page, err := httpx.ParsePage(r)
	if err != nil {
		return err
	}
	status, err := httpx.QEnum(r, "status", "pending", "approved", "rejected", "cancelled")
	if err != nil {
		return err
	}
	party, err := httpx.QInt64(r, "party_id")
	if err != nil {
		return err
	}
	from, to, err := httpx.Range(r)
	if err != nil {
		return err
	}
	p := dbq.ListCreditNoteIDsParams{PartyID: party, FromDate: from, ToDate: to, Lim: page.Limit(), Off: page.Offset()}
	if status != nil {
		s := dbq.ReturnStatus(*status)
		p.Status = &s
	}
	ctx := r.Context()
	rows, err := h.a.Q.ListCreditNoteIDs(ctx, p)
	if err != nil {
		return err
	}
	out := make([]api.CreditNote, 0, len(rows))
	var total int64
	for _, row := range rows {
		total = row.Total
		cn, err := Load(ctx, h.a.Q, row.ID)
		if err != nil {
			return err
		}
		out = append(out, cn)
	}
	httpx.JSON(w, http.StatusOK, httpx.NewList(out, page, total))
	return nil
}

func (h handler) get(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	cn, err := Load(r.Context(), h.a.Q, id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, cn)
	return nil
}

// create records a return against a bill (FR-RT-01): quantity up to the
// quantity sold less earlier returns; values in proportion to the line's
// taxable value (so discounts carry over); GST split as on the bill.
func (h handler) create(w http.ResponseWriter, r *http.Request) error {
	body, err := httpx.ReadBody(r)
	if err != nil {
		return err
	}
	var in api.ReturnInput
	if err := httpx.DecodeBytes(body, &in); err != nil {
		return err
	}
	return idem.Run(w, r, h.a, body, func(ctx context.Context, tx pgx.Tx, q *dbq.Queries) (idem.Result, error) {
		b, err := q.LockBill(ctx, in.BillID)
		if errors.Is(err, pgx.ErrNoRows) {
			return idem.Result{}, httpx.Validation("bill_id", "unknown bill")
		}
		if err != nil {
			return idem.Result{}, err
		}
		if b.Status != dbq.DocStatusActive {
			return idem.Result{}, httpx.InvalidState("returns are only made against active bills")
		}
		if b.SourceBillID != nil {
			return idem.Result{}, httpx.InvalidState("this is an Account copy; make the return against the source Cash bill")
		}
		if in.Settlement == "adjust_ledger" && b.PartyID == nil {
			return idem.Result{}, httpx.Validation("settlement", "a walk-in sale can only be settled by refund")
		}
		if in.Settlement == "refund" {
			if in.Refund == nil {
				return idem.Result{}, httpx.Validation("refund", "refund details are required for settlement refund")
			}
			if err := httpx.Validate(in.Refund); err != nil {
				return idem.Result{}, err
			}
		}
		billLines, err := q.BillLines(ctx, b.ID)
		if err != nil {
			return idem.Result{}, err
		}
		byLine := map[int64]dbq.BillLinesRow{}
		ids := make([]int64, 0, len(billLines))
		itemIDs := make([]int64, 0, len(billLines))
		for _, l := range billLines {
			byLine[l.ID] = l
			ids = append(ids, l.ID)
			itemIDs = append(itemIDs, l.ItemID)
		}
		returned, err := q.ReturnedQty(ctx, ids)
		if err != nil {
			return idem.Result{}, err
		}
		done := map[int64]decimal.Decimal{}
		for _, rq := range returned {
			done[rq.BillLineID] = rq.Qty
		}
		units, err := inventory.UnitFactors(ctx, q, itemIDs)
		if err != nil {
			return idem.Result{}, err
		}
		locs, err := q.ActiveLocations(ctx)
		if err != nil {
			return idem.Result{}, err
		}
		active := map[int16]bool{}
		for _, l := range locs {
			active[l.ID] = true
		}

		type line struct {
			p dbq.InsertCreditNoteLineParams
		}
		var lines []line
		taxable, cgst, sgst, igst := decimal.Zero, decimal.Zero, decimal.Zero, decimal.Zero
		for i, rl := range in.Lines {
			f := fmt.Sprintf("lines[%d]", i)
			bl, ok := byLine[rl.BillLineID]
			if !ok {
				return idem.Result{}, httpx.Validation(f+".bill_line_id", "not a line of this bill")
			}
			qty := num.Parse(rl.Qty)
			base, factor, _, err := inventory.BaseQty(units, f, bl.ItemID, rl.UnitID, qty)
			if err != nil {
				return idem.Result{}, err
			}
			sold := num.Z(bl.BaseQty)
			if done[bl.ID].Add(base).GreaterThan(sold) {
				return idem.Result{}, httpx.Validation(f+".qty", fmt.Sprintf("only %s (base unit) can still be returned",
					sold.Sub(done[bl.ID]).StringFixed(3)))
			}
			done[bl.ID] = done[bl.ID].Add(base)
			loc := bl.LocationID
			if rl.LocationID != nil {
				loc = int16(*rl.LocationID)
			}
			if !active[loc] {
				return idem.Result{}, httpx.Validation(f+".location_id", "unknown or inactive location")
			}
			restock := rl.Restock == nil || *rl.Restock
			lineTaxable := bl.TaxableValue.Mul(base).Div(sold).Round(2)
			c, s, ig := decimal.Zero, decimal.Zero, decimal.Zero
			if b.IsInterstate {
				ig = lineTaxable.Mul(bl.GstRate).Div(hundred).Round(2)
			} else {
				c = lineTaxable.Mul(bl.GstRate).Div(hundred).Div(decimal.NewFromInt(2)).Round(2)
				s = c
			}
			rate := bl.Rate.Div(bl.FactorToBase).Mul(factor).Round(2)
			taxable, cgst, sgst, igst = taxable.Add(lineTaxable), cgst.Add(c), sgst.Add(s), igst.Add(ig)
			lines = append(lines, line{dbq.InsertCreditNoteLineParams{BillLineID: bl.ID, ItemID: bl.ItemID, LocationID: loc,
				Qty: qty, UnitID: int32(rl.UnitID), FactorToBase: factor, Rate: rate, TaxableValue: lineTaxable,
				GstRate: bl.GstRate, Cgst: c, Sgst: s, Igst: ig, Restock: restock}})
		}
		total := taxable.Add(cgst).Add(sgst).Add(igst)
		var refund []byte
		if in.Settlement == "refund" {
			if num.Parse(in.Refund.Amount).GreaterThan(total) {
				return idem.Result{}, httpx.Validation("refund.amount", "is more than the credit note total "+total.StringFixed(2))
			}
			refund, _ = json.Marshal(in.Refund)
		}
		settlement := in.Settlement
		user := auth.MustUserCtx(ctx)
		id, err := q.InsertCreditNote(ctx, dbq.InsertCreditNoteParams{CnDate: h.a.DateOr(httpx.ParseDate(in.CNDate)),
			BillID: b.ID, PartyID: b.PartyID, Reason: strings.TrimSpace(in.Reason), TaxableTotal: taxable, Cgst: cgst,
			Sgst: sgst, Igst: igst, GrandTotal: total, Settlement: &settlement, Refund: refund, CreatedBy: user.ID})
		if err != nil {
			return idem.Result{}, err
		}
		for _, l := range lines {
			l.p.CreditNoteID = id
			if err := q.InsertCreditNoteLine(ctx, l.p); err != nil {
				return idem.Result{}, err
			}
		}
		created, err := Load(ctx, q, id)
		if err != nil {
			return idem.Result{}, err
		}
		if err := audit.Log(ctx, q, "create", "credit_note", audit.ID(id), nil, created); err != nil {
			return idem.Result{}, err
		}
		if user.IsSuperAdmin() {
			if err := h.doApprove(ctx, tx, q, id); err != nil {
				return idem.Result{}, err
			}
		} else {
			role := dbq.UserRoleSuperAdmin
			ref := dbq.DocKindCreditNote
			body := fmt.Sprintf("%s created a return of Rs. %s against %s", user.FullName, total.StringFixed(2), deref(b.BillNo))
			if err := q.InsertNotification(ctx, dbq.InsertNotificationParams{ForRole: &role, Kind: "return_pending",
				Title: "Return waiting for approval", Body: &body, RefType: &ref, RefID: &id}); err != nil {
				return idem.Result{}, err
			}
		}
		out, err := Load(ctx, q, id)
		if err != nil {
			return idem.Result{}, err
		}
		return idem.Result{Status: http.StatusCreated, Body: out, Location: fmt.Sprintf("/api/v1/returns/%d", id)}, nil
	})
}

// doApprove applies an approved return (FR-RT-02).
func (h handler) doApprove(ctx context.Context, tx pgx.Tx, q *dbq.Queries, id int64) error {
	cn, err := q.LockCreditNote(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return httpx.NotFound("return")
	}
	if err != nil {
		return err
	}
	if cn.Status != dbq.ReturnStatusPending {
		return httpx.InvalidState("the return is already " + string(cn.Status))
	}
	b, err := q.LockBill(ctx, cn.BillID)
	if err != nil {
		return err
	}
	if b.Status != dbq.DocStatusActive {
		return httpx.InvalidState("the bill is no longer active")
	}
	no, err := q.NextDocNo(ctx, dbq.NextDocNoParams{DocType: dbq.DocKindCreditNote, DocDate: cn.CnDate})
	if err != nil {
		return err
	}
	fy, err := q.FY(ctx, cn.CnDate)
	if err != nil {
		return err
	}
	user := auth.UserID(ctx)
	ref := dbq.DocKindCreditNote

	// Stock back (Cash bills, restockable lines) at the sale cost.
	if b.AffectsStock {
		lines, err := q.CreditNoteLines(ctx, id)
		if err != nil {
			return err
		}
		for _, l := range lines {
			if !l.Restock {
				continue
			}
			lineID := l.ID
			if err := q.StockIn(ctx, dbq.StockInParams{ItemID: l.ItemID, LocationID: l.LocationID, Qty: num.Z(l.BaseQty),
				Cost: l.SaleUnitCost, Kind: dbq.MovementKindSalesReturn, RefType: &ref, RefID: &id, RefLine: &lineID,
				UserID: user}); err != nil {
				return err
			}
		}
	}
	// Ledger credit for the customer.
	if b.PostsToLedger && b.PartyID != nil {
		narr := "Credit note " + no + " against " + deref(b.BillNo)
		if err := q.InsertLedger(ctx, dbq.InsertLedgerParams{PartyID: *b.PartyID, EntryDate: cn.CnDate, RefType: ref,
			RefID: id, Credit: cn.GrandTotal, Narration: &narr}); err != nil {
			return err
		}
	}
	// Refund paid out.
	var refundID *int64
	if deref(cn.Settlement) == "refund" && len(cn.Refund) > 0 {
		var split api.PaymentSplitInput
		if err := json.Unmarshal(cn.Refund, &split); err != nil {
			return err
		}
		np := ledger.FromSplit(split, dbq.PayDirectionPaid, b.PartyID, cn.CnDate)
		notes := "Refund for credit note " + no
		np.Notes = &notes
		pid, _, err := ledger.Record(ctx, q, np)
		if err != nil {
			return err
		}
		refundID = &pid
	}
	// E-invoice the credit note when the original was e-invoiced (FR-RT-03).
	einv := dbq.EinvStateNotRequired
	if b.EinvStatus == dbq.EinvStateGenerated || b.EinvStatus == dbq.EinvStatePending {
		einv = dbq.EinvStatePending
	}
	if err := q.ApproveCreditNoteRow(ctx, dbq.ApproveCreditNoteRowParams{ID: id, CnNo: &no, Fy: &fy, UserID: user,
		EinvStatus: einv, RefundPaymentID: refundID}); err != nil {
		return err
	}
	if einv == dbq.EinvStatePending {
		if _, err := h.a.River.InsertTx(ctx, tx, einvoice.GenerateArgs{RefType: einvoice.RefCreditNote, RefID: id}, nil); err != nil {
			return err
		}
	}
	return audit.Log(ctx, q, "approve", "credit_note", audit.ID(id), nil, map[string]any{"cn_no": no})
}

func (h handler) approve(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	var out api.CreditNote
	err = h.a.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		if err := h.doApprove(ctx, tx, q, id); err != nil {
			return err
		}
		var err error
		out, err = Load(ctx, q, id)
		return err
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, out)
	return nil
}

func (h handler) reject(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in api.ReasonInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	var out api.CreditNote
	err = h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		cn, err := q.LockCreditNote(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.NotFound("return")
		}
		if err != nil {
			return err
		}
		if cn.Status != dbq.ReturnStatusPending {
			return httpx.InvalidState("only pending returns can be rejected")
		}
		if err := q.RejectCreditNoteRow(ctx, dbq.RejectCreditNoteRowParams{ID: id, Reason: &in.Reason, UserID: auth.UserID(ctx)}); err != nil {
			return err
		}
		if err := audit.Log(ctx, q, "reject", "credit_note", audit.ID(id), nil, map[string]string{"reason": in.Reason}); err != nil {
			return err
		}
		out, err = Load(ctx, q, id)
		return err
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, out)
	return nil
}

func (h handler) pdf(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	cn, err := Load(ctx, h.a.Q, id)
	if err != nil {
		return err
	}
	s, err := h.a.Q.GetSettings(ctx)
	if err != nil {
		return err
	}
	pdf, err := docs.CreditNote(s, cn)
	if err != nil {
		return err
	}
	link, err := docs.Publish(ctx, h.a, fmt.Sprintf("credit-notes/%d-%s.pdf", id, cn.Status), pdf)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, link)
	return nil
}
