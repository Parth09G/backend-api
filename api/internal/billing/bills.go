package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

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
		r.Post("/calculate", httpx.H(h.calculate))
		r.Get("/{id}", httpx.H(h.get))
		r.Put("/{id}", httpx.H(h.update))
		r.Delete("/{id}", httpx.H(h.deleteDraft))
		r.Post("/{id}/finalize", httpx.H(h.finalize))
		r.With(auth.SuperAdmin).Post("/{id}/cancel", httpx.H(h.cancel))
		r.Post("/{id}/copy-to-account", httpx.H(h.copyToAccount))
		r.Get("/{id}/revisions", httpx.H(h.revisions))
		r.Get("/{id}/pdf", httpx.H(h.pdf))
		r.Post("/{id}/share", httpx.H(h.share))
		r.Post("/{id}/einvoice", httpx.H(h.generateEInvoice))
		r.With(auth.SuperAdmin).Post("/{id}/einvoice/cancel", httpx.H(h.cancelEInvoice))
		r.Post("/{id}/ewaybill", httpx.H(h.ewaybill))
	}
}

// ---------------------------------------------------------------- save

type saveOpts struct {
	existing *dbq.Bill // draft being updated / finalized, or active bill being edited
	copyOf   *dbq.Bill // source Cash bill for an Account copy
	reason   string    // edit reason (revision history)
	override bool      // credit-limit override (Super Admin)
}

// save writes a bill in the caller's transaction. Active bills are numbered
// with fn_next_doc_no and posted with fn_post_bill (stock out per line
// location, ledger debit) in the same transaction (API design 4.2).
func (h handler) save(ctx context.Context, tx pgx.Tx, q *dbq.Queries, in api.BillInput, o saveOpts) (int64, error) {
	p, err := prepare(ctx, h.a, q, in, o.copyOf, false)
	if err != nil {
		return 0, err
	}
	user := auth.UserID(ctx)
	activating := in.Status == "active"
	editing := o.existing != nil && o.existing.Status == dbq.DocStatusActive
	var oldIRN *string
	var oldEinv dbq.EinvState

	if editing {
		before, _, err := LoadBill(ctx, h.a, q, o.existing.ID)
		if err != nil {
			return 0, err
		}
		snap, err := jsonBytes(before)
		if err != nil {
			return 0, err
		}
		if err := q.InsertBillRevision(ctx, dbq.InsertBillRevisionParams{BillID: o.existing.ID, Version: o.existing.Version,
			Snapshot: snap, Reason: o.reason, EditedBy: *user}); err != nil {
			return 0, err
		}
		// Reverse the old version: stock and ledger (fn_unpost_bill), and the
		// payments taken on the bill screen (they are re-entered with the edit).
		if err := q.UnpostBill(ctx, dbq.UnpostBillParams{BillID: o.existing.ID, UserID: *user}); err != nil {
			return 0, err
		}
		if err := cancelScreenPayments(ctx, h.a, q, o.existing.ID, "bill edited: "+o.reason); err != nil {
			return 0, err
		}
		if o.existing.EinvStatus == dbq.EinvStateGenerated {
			oldIRN, oldEinv = o.existing.Irn, o.existing.EinvStatus
		}
	}
	if o.existing != nil {
		if err := q.DeleteBillLines(ctx, o.existing.ID); err != nil {
			return 0, err
		}
		if err := q.DeleteBillCharges(ctx, o.existing.ID); err != nil {
			return 0, err
		}
	}

	if activating {
		short, err := inventory.CheckShortages(ctx, q, p.needs(), true)
		if err != nil {
			return 0, err
		}
		if len(short) > 0 {
			return 0, &inventory.ShortageError{Lines: short}
		}
		if w, err := creditProblem(ctx, q, p); err != nil {
			return 0, err
		} else if w != nil && !(o.override && auth.MustUserCtx(ctx).IsSuperAdmin()) {
			return 0, creditError(w)
		}
	}

	einv := dbq.EinvStateNotRequired
	if activating && p.einvEligible() {
		einv = dbq.EinvStatePending
	}
	c := p.calc
	var id int64
	if o.existing == nil {
		ip := dbq.InsertBillParams{BillType: p.billType, BillDate: p.date, Status: dbq.DocStatusDraft, PartyID: p.partyID,
			CustName: p.custName, CustGstin: p.gstin, CustAddress: p.address, CustMobile: p.mobile, SiteAddress: p.site,
			PlaceOfSupply: p.pos, IsInterstate: p.interstate, VehicleNo: trimmed(in.VehicleNo), AffectsStock: p.affectsStock(),
			PostsToLedger: p.postsToLedger(), ItemsTaxable: c.ItemsTaxable, ChargesTaxable: c.ChargesTaxable,
			BillDiscount: c.BillDiscount, TaxableTotal: c.TaxableTotal, Cgst: c.CGST, Sgst: c.SGST, Igst: c.IGST,
			RoundOff: c.RoundOff, GrandTotal: c.GrandTotal, EinvStatus: einv, Notes: trimmed(in.Notes), CreatedBy: *user}
		if o.copyOf != nil {
			ip.SourceBillID = &o.copyOf.ID
		}
		if activating {
			no, fy, err := number(ctx, q, p.billType, p.date)
			if err != nil {
				return 0, err
			}
			ip.BillNo, ip.Fy, ip.Status = &no, &fy, dbq.DocStatusActive
		}
		if id, err = q.InsertBill(ctx, ip); err != nil {
			return 0, err
		}
	} else {
		id = o.existing.ID
		if err := q.UpdateBillContent(ctx, dbq.UpdateBillContentParams{ID: id, BillDate: p.date, PartyID: p.partyID,
			CustName: p.custName, CustGstin: p.gstin, CustAddress: p.address, CustMobile: p.mobile, SiteAddress: p.site,
			PlaceOfSupply: p.pos, IsInterstate: p.interstate, VehicleNo: trimmed(in.VehicleNo), ItemsTaxable: c.ItemsTaxable,
			ChargesTaxable: c.ChargesTaxable, BillDiscount: c.BillDiscount, TaxableTotal: c.TaxableTotal, Cgst: c.CGST,
			Sgst: c.SGST, Igst: c.IGST, RoundOff: c.RoundOff, GrandTotal: c.GrandTotal, EinvStatus: einv,
			Notes: trimmed(in.Notes), UserID: user}); err != nil {
			return 0, err
		}
		if activating && o.existing.Status == dbq.DocStatusDraft {
			no, fy, err := number(ctx, q, p.billType, p.date)
			if err != nil {
				return 0, err
			}
			if err := q.ActivateBill(ctx, dbq.ActivateBillParams{ID: id, BillNo: &no, Fy: &fy, UserID: user}); err != nil {
				return 0, err
			}
		}
	}
	for i, l := range p.lines {
		if _, err := q.InsertBillLine(ctx, dbq.InsertBillLineParams{BillID: id, LineNo: int16(i + 1), ItemID: l.itemID,
			LocationID: l.locationID, Description: l.desc, HsnCode: l.hsn, Qty: l.qty, UnitID: l.unitID, FactorToBase: l.factor,
			Rate: l.rate, DiscountPct: l.pct, DiscountAmt: l.amt, TaxableValue: l.out.Taxable, GstRate: l.gst,
			Cgst: l.out.CGST, Sgst: l.out.SGST, Igst: l.out.IGST, LineTotal: l.out.Total}); err != nil {
			return 0, err
		}
	}
	for _, ch := range p.charges {
		if err := q.InsertBillCharge(ctx, dbq.InsertBillChargeParams{BillID: id, Kind: ch.kind, Description: ch.desc,
			SacCode: ch.sac, Amount: ch.amount, GstRate: ch.gst, Cgst: ch.out.CGST, Sgst: ch.out.SGST, Igst: ch.out.IGST}); err != nil {
			return 0, err
		}
	}

	if activating {
		if err := q.PostBill(ctx, dbq.PostBillParams{BillID: id, UserID: *user}); err != nil {
			return 0, err
		}
		for _, pay := range in.Payments {
			np := ledger.FromSplit(pay, dbq.PayDirectionReceived, p.partyID, p.date)
			np.BillID = &id
			payID, _, err := ledger.Record(ctx, q, np)
			if err != nil {
				return 0, err
			}
			if err := ledger.Allocate(ctx, q, payID, docKindFor(p.billType), id, np.Amount); err != nil {
				return 0, err
			}
		}
		if _, err := q.RefreshBillPaid(ctx, id); err != nil {
			return 0, err
		}
	}
	if editing {
		if _, err := q.BumpBillVersion(ctx, dbq.BumpBillVersionParams{ID: id, UserID: user}); err != nil {
			return 0, err
		}
	}

	// E-invoice: an edit inside the window cancels the old IRN first and
	// then regenerates (FR-ED-03); a new B2B bill queues generation.
	switch {
	case oldEinv == dbq.EinvStateGenerated && oldIRN != nil:
		if einv != dbq.EinvStatePending {
			einv = dbq.EinvStateNotRequired
		}
		if _, err := h.a.River.InsertTx(ctx, tx, einvoice.CancelArgs{RefType: einvoice.RefBill, RefID: id, IRN: *oldIRN,
			Reason: "Data entry mistake", Regenerate: einv == dbq.EinvStatePending}, nil); err != nil {
			return 0, err
		}
		if einv == dbq.EinvStatePending {
			// keep the old IRN fields until the cancel job clears them
			if err := q.SetBillEInvoice(ctx, dbq.SetBillEInvoiceParams{ID: id, EinvStatus: dbq.EinvStatePending}); err != nil {
				return 0, err
			}
		}
	case einv == dbq.EinvStatePending:
		if _, err := h.a.River.InsertTx(ctx, tx, einvoice.GenerateArgs{RefType: einvoice.RefBill, RefID: id}, nil); err != nil {
			return 0, err
		}
	}
	return id, nil
}

func number(ctx context.Context, q *dbq.Queries, t dbq.BillKind, date timeT) (string, string, error) {
	no, err := q.NextDocNo(ctx, dbq.NextDocNoParams{DocType: docKindFor(t), DocDate: date})
	if err != nil {
		return "", "", err
	}
	fy, err := q.FY(ctx, date)
	return no, fy, err
}

// cancelScreenPayments cancels the payments taken on a bill's screen and
// reverses them in the ledger (edit re-enters them; cancel refunds them).
func cancelScreenPayments(ctx context.Context, a *app.App, q *dbq.Queries, billID int64, reason string) error {
	pays, err := q.BillScreenPayments(ctx, &billID)
	if err != nil {
		return err
	}
	for _, p := range pays {
		if err := q.CancelPaymentRow(ctx, dbq.CancelPaymentRowParams{ID: p.ID, Reason: &reason, UserID: auth.UserID(ctx)}); err != nil {
			return err
		}
		if p.ChequeStatus != nil && *p.ChequeStatus == dbq.ChequeStateBounced {
			continue // already reversed
		}
		if err := ledger.Reverse(ctx, q, p, a.Today()); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------- handlers

func (h handler) respond(ctx context.Context, q *dbq.Queries, id int64, status int, action string, before any) (idem.Result, error) {
	b, _, err := LoadBill(ctx, h.a, q, id)
	if err != nil {
		return idem.Result{}, err
	}
	if err := audit.Log(ctx, q, action, "bill", audit.ID(id), before, b); err != nil {
		return idem.Result{}, err
	}
	res := idem.Result{Status: status, Body: b, ETag: b.Version}
	if status == http.StatusCreated {
		res.Location = fmt.Sprintf("/api/v1/bills/%d", id)
	}
	return res, nil
}

// raiseShortage converts a stock shortage into 422 and, in a separate
// transaction, alerts the Super Admin for each short line (FR-CB-03).
func (h handler) raiseShortage(ctx context.Context, err error) error {
	var se *inventory.ShortageError
	if !errors.As(err, &se) {
		return err
	}
	alerted := true
	user := auth.UserID(ctx)
	aerr := h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		for _, l := range se.Lines {
			if err := q.AlertShortage(ctx, dbq.AlertShortageParams{ItemID: l.ItemID, LocationID: int16(l.LocationID),
				Required: l.Required.D(), UserID: *user}); err != nil {
				return err
			}
		}
		return nil
	})
	if aerr != nil {
		slog.ErrorContext(ctx, "shortage alert", "err", aerr)
		alerted = false
	}
	return inventory.ShortageProblem(se.Lines, alerted)
}

func (h handler) create(w http.ResponseWriter, r *http.Request) error {
	body, err := httpx.ReadBody(r)
	if err != nil {
		return err
	}
	var in api.BillInput
	if err := httpx.DecodeBytes(body, &in); err != nil {
		return err
	}
	ctx := r.Context()
	err = idem.Run(w, r, h.a, body, func(ctx context.Context, tx pgx.Tx, q *dbq.Queries) (idem.Result, error) {
		id, err := h.save(ctx, tx, q, in, saveOpts{override: in.OverrideCreditLimit})
		if err != nil {
			return idem.Result{}, err
		}
		return h.respond(ctx, q, id, http.StatusCreated, "create", nil)
	})
	return h.raiseShortage(ctx, err)
}

// calculate previews taxes, totals, stock and credit warnings; nothing is saved.
func (h handler) calculate(w http.ResponseWriter, r *http.Request) error {
	var in api.BillInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	p, err := prepare(ctx, h.a, h.a.Q, in, nil, true)
	if err != nil {
		return err
	}
	out := api.BillCalculation{Lines: p.apiLines(), Charges: p.apiCharges(), Totals: totals(p.calc, p.paid),
		StockWarnings: []api.ShortLine{}}
	short, err := inventory.CheckShortages(ctx, h.a.Q, p.needs(), false)
	if err != nil {
		return err
	}
	if short != nil {
		out.StockWarnings = short
	}
	if out.CreditWarning, err = creditProblem(ctx, h.a.Q, p); err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, out)
	return nil
}

func (h handler) get(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	b, _, err := LoadBill(r.Context(), h.a, h.a.Q, id)
	if err != nil {
		return err
	}
	httpx.ETag(w, b.Version)
	httpx.JSON(w, http.StatusOK, b)
	return nil
}

// lockForChange locks a bill and checks the If-Match version (412).
func lockForChange(ctx context.Context, q *dbq.Queries, id int64, version int32) (dbq.Bill, error) {
	b, err := q.LockBill(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, httpx.NotFound("bill")
	}
	if err != nil {
		return b, err
	}
	if b.Version != version {
		return b, &httpx.Problem{Status: http.StatusPreconditionFailed, Code: httpx.CodeVersionMismatch,
			Detail: fmt.Sprintf("the bill is at version %d; reload it", b.Version)}
	}
	return b, nil
}

// checkEditable applies the edit rules for an active bill (FR-ED-01..04).
func (h handler) checkEditable(ctx context.Context, q *dbq.Queries, b dbq.Bill, newDate *timeT) error {
	s, err := q.GetSettings(ctx)
	if err != nil {
		return err
	}
	if periodLocked(b.BillDate, &s) || (newDate != nil && periodLocked(*newDate, &s)) {
		return httpx.Conflict(httpx.CodeEditNotAllowed, "the period is locked (exported to Tally / GST filed)")
	}
	has, err := q.BillHasReturns(ctx, b.ID)
	if err != nil {
		return err
	}
	if has {
		return httpx.Conflict(httpx.CodeEditNotAllowed, "the bill has returns; use a credit note instead")
	}
	row := dbq.GetBillRow{EinvStatus: b.EinvStatus, AckDate: b.AckDate}
	if windowClosed(row, &s, h.a.Now()) {
		return httpx.Conflict(httpx.CodeEInvoiceWindow, "the IRN is older than the edit window; create a credit note instead")
	}
	return nil
}

// update saves a draft (any role) or edits an active bill (Super Admin,
// reason required, revision stored, IRN cancelled and regenerated).
func (h handler) update(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	version, err := httpx.IfMatch(r)
	if err != nil {
		return err
	}
	var in api.BillEditInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	var res idem.Result
	err = h.a.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		b, err := lockForChange(ctx, q, id, version)
		if err != nil {
			return err
		}
		if string(b.BillType) != in.BillType {
			return httpx.Validation("bill_type", "the bill type cannot change")
		}
		o := saveOpts{existing: &b, reason: strings.TrimSpace(in.Reason), override: in.OverrideCreditLimit}
		if b.SourceBillID != nil {
			src, err := q.LockBill(ctx, *b.SourceBillID)
			if err != nil {
				return err
			}
			o.copyOf = &src
		}
		action := "update"
		switch b.Status {
		case dbq.DocStatusCancelled:
			return httpx.InvalidState("a cancelled bill cannot be edited")
		case dbq.DocStatusActive:
			if !auth.MustUser(r).IsSuperAdmin() {
				return httpx.Conflict(httpx.CodeEditNotAllowed, "only the Super Admin can edit a saved bill (O-02)")
			}
			if len(o.reason) < 3 {
				return httpx.Validation("reason", "a reason is required to edit a saved bill")
			}
			if in.Status != "active" {
				return httpx.Validation("status", "a saved bill stays active; cancel it instead")
			}
			if err := h.checkEditable(ctx, q, b, httpx.ParseDate(in.BillDate)); err != nil {
				return err
			}
			action = "edit"
		default:
			if in.Status == "active" {
				action = "finalize"
			}
		}
		before, _, err := LoadBill(ctx, h.a, q, id)
		if err != nil {
			return err
		}
		if _, err := h.save(ctx, tx, q, in.BillInput, o); err != nil {
			return err
		}
		res, err = h.respond(ctx, q, id, http.StatusOK, action, map[string]any{"bill": before, "reason": o.reason})
		return err
	})
	if err != nil {
		return h.raiseShortage(ctx, err)
	}
	httpx.ETag(w, res.ETag)
	httpx.JSON(w, res.Status, res.Body)
	return nil
}

func (h handler) deleteDraft(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	err = h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		b, err := q.LockBill(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.NotFound("bill")
		}
		if err != nil {
			return err
		}
		if b.Status != dbq.DocStatusDraft {
			return httpx.InvalidState("only drafts can be deleted; cancel a saved bill instead")
		}
		before, _, err := LoadBill(ctx, h.a, q, id)
		if err != nil {
			return err
		}
		if _, err := q.DeleteDraftBill(ctx, id); err != nil {
			return err
		}
		return audit.Log(ctx, q, "delete", "bill", audit.ID(id), before, nil)
	})
	if err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}

// inputFromBill rebuilds a request from a stored bill (finalize, copy).
func inputFromBill(ctx context.Context, q *dbq.Queries, b dbq.Bill) (api.BillInput, error) {
	lines, err := q.BillLines(ctx, b.ID)
	if err != nil {
		return api.BillInput{}, err
	}
	charges, err := q.BillCharges(ctx, b.ID)
	if err != nil {
		return api.BillInput{}, err
	}
	date := b.BillDate.Format("2006-01-02")
	in := api.BillInput{BillType: string(b.BillType), Status: "active", BillDate: &date, PlaceOfSupply: b.PlaceOfSupply,
		VehicleNo: b.VehicleNo, Notes: b.Notes,
		Customer: api.CustomerInput{PartyID: b.PartyID, GSTIN: trimmed(b.CustGstin), BillingAddress: b.CustAddress,
			Mobile: b.CustMobile, SiteAddress: b.SiteAddress}}
	if b.PartyID == nil {
		name := b.CustName
		in.Customer.WalkInName = &name
	}
	if !b.BillDiscount.IsZero() {
		bd := b.BillDiscount.StringFixed(2)
		in.BillDiscount = &bd
	}
	for _, l := range lines {
		loc := int64(l.LocationID)
		desc := l.Description
		pct := num.Trim(l.DiscountPct)
		amt := l.DiscountAmt.StringFixed(2)
		in.Lines = append(in.Lines, api.BillLineInput{ItemID: l.ItemID, LocationID: &loc, Qty: l.Qty.String(),
			UnitID: int64(l.UnitID), Rate: l.Rate.StringFixed(2), DiscountPct: &pct, DiscountAmt: &amt, Description: &desc})
	}
	for _, c := range charges {
		desc := c.Description
		rate := num.Trim(c.GstRate)
		in.Charges = append(in.Charges, api.BillChargeInput{Kind: string(c.Kind), Description: &desc,
			Amount: c.Amount.StringFixed(2), GSTRate: &rate, SACCode: c.SacCode})
	}
	return in, nil
}

// finalize turns a draft into an active bill with the draft's content.
func (h handler) finalize(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	err = idem.Run(w, r, h.a, nil, func(ctx context.Context, tx pgx.Tx, q *dbq.Queries) (idem.Result, error) {
		b, err := q.LockBill(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return idem.Result{}, httpx.NotFound("bill")
		}
		if err != nil {
			return idem.Result{}, err
		}
		if b.Status != dbq.DocStatusDraft {
			return idem.Result{}, httpx.InvalidState("only drafts can be finalized")
		}
		in, err := inputFromBill(ctx, q, b)
		if err != nil {
			return idem.Result{}, err
		}
		o := saveOpts{existing: &b, override: auth.MustUserCtx(ctx).IsSuperAdmin()}
		if b.SourceBillID != nil {
			src, err := q.LockBill(ctx, *b.SourceBillID)
			if err != nil {
				return idem.Result{}, err
			}
			if src.Status != dbq.DocStatusActive {
				return idem.Result{}, httpx.InvalidState("the source Cash bill is no longer active")
			}
			o.copyOf = &src
		}
		if _, err := h.save(ctx, tx, q, in, o); err != nil {
			return idem.Result{}, err
		}
		return h.respond(ctx, q, id, http.StatusOK, "finalize", nil)
	})
	return h.raiseShortage(ctx, err)
}

// cancel reverses stock and ledger and keeps the number (FR-ED-05).
// Payments taken on the bill screen are cancelled (refunded); receipts
// recorded later go back on account.
func (h handler) cancel(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	version, err := httpx.IfMatch(r)
	if err != nil {
		return err
	}
	var in api.ReasonInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	var res idem.Result
	err = h.a.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		b, err := lockForChange(ctx, q, id, version)
		if err != nil {
			return err
		}
		if b.Status != dbq.DocStatusActive {
			return httpx.InvalidState("only active bills can be cancelled (delete a draft instead)")
		}
		if _, err := q.ActiveAccountCopy(ctx, &b.ID); err == nil {
			return httpx.InvalidState("cancel the Account copy of this bill first")
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		s, err := q.GetSettings(ctx)
		if err != nil {
			return err
		}
		if has, err := q.BillHasReturns(ctx, id); err != nil {
			return err
		} else if has {
			return httpx.InvalidState("the bill has returns and cannot be cancelled")
		}
		if windowClosed(dbq.GetBillRow{EinvStatus: b.EinvStatus, AckDate: b.AckDate}, &s, h.a.Now()) {
			return httpx.Conflict(httpx.CodeEInvoiceWindow, "the IRN is older than the cancel window; create a credit note instead")
		}
		before, _, err := LoadBill(ctx, h.a, q, id)
		if err != nil {
			return err
		}
		user := auth.UserID(ctx)
		if err := q.UnpostBill(ctx, dbq.UnpostBillParams{BillID: id, UserID: *user}); err != nil {
			return err
		}
		if err := cancelScreenPayments(ctx, h.a, q, id, "bill cancelled: "+in.Reason); err != nil {
			return err
		}
		if err := q.DetachBillAllocations(ctx, dbq.DetachBillAllocationsParams{RefType: refKind(b.BillType), BillID: id}); err != nil {
			return err
		}
		if _, err := q.RefreshBillPaid(ctx, id); err != nil {
			return err
		}
		if err := q.CancelBillRow(ctx, dbq.CancelBillRowParams{ID: id, Reason: &in.Reason, UserID: user}); err != nil {
			return err
		}
		if b.EinvStatus == dbq.EinvStateGenerated && b.Irn != nil {
			if _, err := h.a.River.InsertTx(ctx, tx, einvoice.CancelArgs{RefType: einvoice.RefBill, RefID: id, IRN: *b.Irn,
				Reason: in.Reason}, nil); err != nil {
				return err
			}
		}
		res, err = h.respond(ctx, q, id, http.StatusOK, "cancel", map[string]any{"bill": before, "reason": in.Reason})
		return err
	})
	if err != nil {
		return err
	}
	httpx.ETag(w, res.ETag)
	httpx.JSON(w, res.Status, res.Body)
	return nil
}

// copyToAccount creates an Account bill from an active Cash bill. The copy
// never changes stock and does not post to the ledger again (BR-04).
func (h handler) copyToAccount(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	body, err := httpx.ReadBody(r)
	if err != nil {
		return err
	}
	var in api.CopyToAccountInput
	if err := httpx.DecodeBytes(body, &in); err != nil {
		return err
	}
	return idem.Run(w, r, h.a, body, func(ctx context.Context, tx pgx.Tx, q *dbq.Queries) (idem.Result, error) {
		src, err := q.LockBill(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return idem.Result{}, httpx.NotFound("bill")
		}
		if err != nil {
			return idem.Result{}, err
		}
		if src.BillType != dbq.BillKindCash || src.Status != dbq.DocStatusActive || src.SourceBillID != nil {
			return idem.Result{}, httpx.InvalidState("only active Cash bills can be copied to an Account bill")
		}
		if src.PartyID == nil {
			return idem.Result{}, httpx.InvalidState("an Account bill needs a named customer; this Cash bill is a walk-in sale")
		}
		if _, err := q.ActiveAccountCopy(ctx, &src.ID); err == nil {
			return idem.Result{}, httpx.InvalidState("this Cash bill already has an active Account copy")
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return idem.Result{}, err
		}
		bin, err := inputFromBill(ctx, q, src)
		if err != nil {
			return idem.Result{}, err
		}
		bin.BillType = "account"
		bin.Status = "active"
		if in.Status != nil {
			bin.Status = *in.Status
		}
		today := h.a.Today().Format("2006-01-02")
		bin.BillDate = &today
		if in.BillDate != nil {
			bin.BillDate = in.BillDate
		}
		if in.Notes != nil {
			bin.Notes = in.Notes
		}
		newID, err := h.save(ctx, tx, q, bin, saveOpts{copyOf: &src})
		if err != nil {
			return idem.Result{}, err
		}
		return h.respond(ctx, q, newID, http.StatusCreated, "copy_to_account", map[string]any{"source_bill_id": id})
	})
}

func (h handler) revisions(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	if _, err := h.a.Q.GetBill(ctx, id); err != nil {
		return httpx.NotFound("bill")
	}
	rows, err := h.a.Q.BillRevisions(ctx, id)
	if err != nil {
		return err
	}
	out := make([]api.BillRevision, len(rows))
	for i, rv := range rows {
		out[i] = api.BillRevision{Version: rv.Version, Reason: rv.Reason, EditedByName: rv.EditedByName,
			EditedAt: rv.EditedAt, Snapshot: rv.Snapshot}
	}
	httpx.JSON(w, http.StatusOK, out)
	return nil
}

// pdfLink renders and publishes the invoice PDF for the bill's version.
func (h handler) pdfLink(ctx context.Context, id int64, copyKind string) (api.SignedURL, api.Bill, error) {
	b, _, err := LoadBill(ctx, h.a, h.a.Q, id)
	if err != nil {
		return api.SignedURL{}, b, err
	}
	if b.Status == "draft" {
		return api.SignedURL{}, b, httpx.InvalidState("drafts cannot be printed; finalize the bill first")
	}
	if b.EInvoice.Status == "pending" || b.EInvoice.Status == "failed" {
		return api.SignedURL{}, b, httpx.InvalidState("waiting for the e-invoice (IRN); print after it is generated")
	}
	s, err := h.a.Q.GetSettings(ctx)
	if err != nil {
		return api.SignedURL{}, b, err
	}
	pdf, err := docs.Invoice(s, b, copyKind)
	if err != nil {
		return api.SignedURL{}, b, err
	}
	path := fmt.Sprintf("bills/%d/v%d-%s-%s.pdf", b.ID, b.Version, b.Status, copyKind)
	link, err := docs.Publish(ctx, h.a, path, pdf)
	return link, b, err
}

func (h handler) pdf(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	copyKind, err := httpx.QEnum(r, "copy", "original", "duplicate", "triplicate")
	if err != nil {
		return err
	}
	kind := "original"
	if copyKind != nil {
		kind = *copyKind
	}
	link, _, err := h.pdfLink(r.Context(), id, kind)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, link)
	return nil
}

// share returns a WhatsApp link with the PDF URL and message text.
func (h handler) share(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	link, b, err := h.pdfLink(r.Context(), id, "original")
	if err != nil {
		return err
	}
	shop := ""
	if s, err := h.a.Q.GetSettings(r.Context()); err == nil {
		shop = s.ShopName
	}
	msg := fmt.Sprintf("Dear %s, your invoice %s dated %s for Rs. %s from %s: %s", b.Customer.Name, deref(b.BillNo),
		docs.DMY(b.BillDate.T()), docs.INR(b.Totals.GrandTotal.D()), shop, link.URL)
	phone := ""
	if b.Customer.Mobile != nil {
		phone = digits(*b.Customer.Mobile)
		if len(phone) == 10 {
			phone = "91" + phone
		}
	}
	httpx.JSON(w, http.StatusOK, api.ShareResult{WhatsAppURL: "https://wa.me/" + phone + "?text=" + url.QueryEscape(msg), PDFURL: link.URL})
	return nil
}

func digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// generateEInvoice queues (or retries) IRN generation (FR-EI-01/02).
func (h handler) generateEInvoice(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	var out api.EInvoice
	err = h.a.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		b, err := q.LockBill(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.NotFound("bill")
		}
		if err != nil {
			return err
		}
		s, err := q.GetSettings(ctx)
		if err != nil {
			return err
		}
		switch {
		case b.Status != dbq.DocStatusActive:
			return httpx.InvalidState("only active bills can be e-invoiced")
		case b.SourceBillID != nil:
			return httpx.InvalidState("an Account copy is not e-invoiced; the source Cash bill is")
		case trimmed(b.CustGstin) == nil:
			return httpx.InvalidState("B2C bills (no customer GSTIN) do not need an e-invoice")
		case !s.EinvoiceEnabled:
			return httpx.InvalidState("e-invoicing is turned off in settings")
		case b.EinvStatus == dbq.EinvStateGenerated:
			return httpx.InvalidState("the IRN is already generated")
		}
		if err := q.SetBillEInvoice(ctx, dbq.SetBillEInvoiceParams{ID: id, EinvStatus: dbq.EinvStatePending}); err != nil {
			return err
		}
		if _, err := h.a.River.InsertTx(ctx, tx, einvoice.GenerateArgs{RefType: einvoice.RefBill, RefID: id}, nil); err != nil {
			return err
		}
		if err := audit.Log(ctx, q, "einvoice_generate", "bill", audit.ID(id), nil, nil); err != nil {
			return err
		}
		bill, _, err := LoadBill(ctx, h.a, q, id)
		out = bill.EInvoice
		return err
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusAccepted, out)
	return nil
}

// cancelEInvoice queues IRN cancellation inside the window (FR-EI-03).
func (h handler) cancelEInvoice(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in api.ReasonInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	var out api.EInvoice
	err = h.a.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		b, err := q.LockBill(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.NotFound("bill")
		}
		if err != nil {
			return err
		}
		if b.EinvStatus != dbq.EinvStateGenerated || b.Irn == nil {
			return httpx.InvalidState("there is no generated IRN to cancel")
		}
		s, err := q.GetSettings(ctx)
		if err != nil {
			return err
		}
		if windowClosed(dbq.GetBillRow{EinvStatus: b.EinvStatus, AckDate: b.AckDate}, &s, h.a.Now()) {
			return httpx.Conflict(httpx.CodeEInvoiceWindow, "an IRN can only be cancelled within 24 hours; create a credit note")
		}
		if _, err := h.a.River.InsertTx(ctx, tx, einvoice.CancelArgs{RefType: einvoice.RefBill, RefID: id, IRN: *b.Irn,
			Reason: in.Reason}, nil); err != nil {
			return err
		}
		if err := audit.Log(ctx, q, "einvoice_cancel", "bill", audit.ID(id), nil, map[string]string{"reason": in.Reason}); err != nil {
			return err
		}
		bill, _, err := LoadBill(ctx, h.a, q, id)
		out = bill.EInvoice
		return err
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusAccepted, out)
	return nil
}

// ewaybill queues e-way bill generation (FR-EI-04).
func (h handler) ewaybill(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in api.EWayBillInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	var out api.EInvoice
	err = h.a.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		b, err := LoadBillForAction(ctx, h.a, q, id)
		if err != nil {
			return err
		}
		if !contains(b.AllowedActions, "generate_ewaybill") {
			return httpx.InvalidState("an e-way bill cannot be generated for this bill now (needs an active bill, IRN first for B2B, no existing EWB)")
		}
		mode := "road"
		if in.TransportMode != nil {
			mode = *in.TransportMode
		}
		args := einvoice.EWBArgs{BillID: id, VehicleNo: strings.ToUpper(in.VehicleNo), TransportMode: mode, DistanceKm: in.DistanceKm}
		if in.TransporterID != nil {
			args.TransporterID = *in.TransporterID
		}
		if _, err := h.a.River.InsertTx(ctx, tx, args, nil); err != nil {
			return err
		}
		if err := audit.Log(ctx, q, "ewaybill_generate", "bill", audit.ID(id), nil, args); err != nil {
			return err
		}
		out = b.EInvoice
		return nil
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusAccepted, out)
	return nil
}

// LoadBillForAction loads a bill after locking it.
func LoadBillForAction(ctx context.Context, a *app.App, q *dbq.Queries, id int64) (api.Bill, error) {
	if _, err := q.LockBill(ctx, id); errors.Is(err, pgx.ErrNoRows) {
		return api.Bill{}, httpx.NotFound("bill")
	} else if err != nil {
		return api.Bill{}, err
	}
	b, _, err := LoadBill(ctx, a, q, id)
	return b, err
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- list

func (h handler) list(w http.ResponseWriter, r *http.Request) error {
	page, err := httpx.ParsePage(r)
	if err != nil {
		return err
	}
	billType, err := httpx.QEnum(r, "bill_type", "cash", "account")
	if err != nil {
		return err
	}
	status, err := httpx.QEnum(r, "status", "draft", "active", "cancelled")
	if err != nil {
		return err
	}
	payStatus, err := httpx.QEnum(r, "payment_status", "paid", "partial", "unpaid")
	if err != nil {
		return err
	}
	einv, err := httpx.QEnum(r, "einvoice_status", "not_required", "pending", "generated", "failed", "cancelled")
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
	unlinked, err := httpx.QBool(r, "unlinked_account_bills")
	if err != nil {
		return err
	}
	sort, desc, err := httpx.Sort(r, "-bill_date", "bill_date", "bill_no", "grand_total", "created_at")
	if err != nil {
		return err
	}
	p := dbq.ListBillsParams{PaymentStatus: payStatus, PartyID: party, FromDate: from, ToDate: to,
		Q: httpx.QString(r, "q"), Unlinked: unlinked, Sort: sort, Descending: desc, Lim: page.Limit(), Off: page.Offset()}
	if billType != nil {
		t := dbq.BillKind(*billType)
		p.BillType = &t
	}
	if status != nil {
		s := dbq.DocStatus(*status)
		p.Status = &s
	}
	if einv != nil {
		e := dbq.EinvState(*einv)
		p.EinvStatus = &e
	}
	if loc != nil {
		l := int16(*loc)
		p.LocationID = &l
	}
	rows, err := h.a.Q.ListBills(r.Context(), p)
	if err != nil {
		return err
	}
	out := make([]api.BillSummary, len(rows))
	var total int64
	for i, b := range rows {
		total = b.Total
		bal := num.Z(b.BalanceDue)
		ps := "unpaid"
		switch {
		case !bal.IsPositive():
			ps = "paid"
		case b.AmountPaid.IsPositive():
			ps = "partial"
		}
		out[i] = api.BillSummary{ID: b.ID, BillType: string(b.BillType), BillNo: b.BillNo, BillDate: api.Date(b.BillDate),
			Status: string(b.Status), CustomerName: b.CustName, PartyID: b.PartyID, GrandTotal: api.M(b.GrandTotal),
			BalanceDue: api.M(bal), PaymentStatus: ps, EInvoiceStatus: string(b.EinvStatus), IsAccountCopy: b.SourceBillID != nil,
			SourceBillID: b.SourceBillID, AccountCopyID: nonZero(b.AccountCopyID), Version: b.Version, CreatedByName: b.CreatedByName}
	}
	httpx.JSON(w, http.StatusOK, httpx.NewList(out, page, total))
	return nil
}

func nonZero(n int64) *int64 {
	if n == 0 {
		return nil
	}
	return &n
}
