package ledger

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"billing-api/internal/api"
	"billing-api/internal/app"
	"billing-api/internal/audit"
	"billing-api/internal/auth"
	"billing-api/internal/db/dbq"
	"billing-api/internal/docs"
	"billing-api/internal/httpx"
	"billing-api/internal/idem"
	"billing-api/internal/num"
)

func PaymentRoutes(a *app.App) func(chi.Router) {
	h := handler{a}
	return func(r chi.Router) {
		r.Get("/", httpx.H(h.listPayments))
		r.Post("/", httpx.H(h.createPayment))
		r.Get("/{id}", httpx.H(h.getPayment))
		r.Post("/{id}/cheque-status", httpx.H(h.chequeStatus))
		r.With(auth.SuperAdmin).Post("/{id}/cancel", httpx.H(h.cancelPayment))
		r.Get("/{id}/receipt", httpx.H(h.receipt))
	}
}

// ---------------------------------------------------------------- posting helpers

// NewPayment is a payment to record (receipt or payment voucher).
type NewPayment struct {
	Direction  dbq.PayDirection
	PartyID    *int64 // nil only for walk-in money taken on a bill
	Date       time.Time
	Mode       dbq.PayMode
	Amount     decimal.Decimal
	Reference  *string
	ChequeNo   *string
	ChequeDate *time.Time
	BankName   *string
	Notes      *string
	BillID     *int64 // taken on the bill screen
}

// FromSplit converts a payment split from a request.
func FromSplit(s api.PaymentSplitInput, dir dbq.PayDirection, party *int64, date time.Time) NewPayment {
	return NewPayment{Direction: dir, PartyID: party, Date: date, Mode: dbq.PayMode(s.Mode), Amount: num.Parse(s.Amount),
		Reference: blank(s.Reference), ChequeNo: blank(s.ChequeNo), ChequeDate: httpx.ParseDate(s.ChequeDate),
		BankName: blank(s.BankName)}
}

func docKind(dir dbq.PayDirection) dbq.DocKind {
	if dir == dbq.PayDirectionPaid {
		return dbq.DocKindPaymentVoucher
	}
	return dbq.DocKindReceipt
}

// Record numbers and inserts a payment and posts it to the party ledger:
// received = credit (customer owes less), paid = debit (we owe the dealer less).
func Record(ctx context.Context, q *dbq.Queries, p NewPayment) (int64, string, error) {
	kind := docKind(p.Direction)
	docNo, err := q.NextDocNo(ctx, dbq.NextDocNoParams{DocType: kind, DocDate: p.Date})
	if err != nil {
		return 0, "", err
	}
	fy, err := q.FY(ctx, p.Date)
	if err != nil {
		return 0, "", err
	}
	var cheque *dbq.ChequeState
	if p.Mode == dbq.PayModeCheque {
		s := dbq.ChequeStatePending
		cheque = &s
	}
	id, err := q.InsertPayment(ctx, dbq.InsertPaymentParams{Direction: p.Direction, DocNo: docNo, Fy: fy, PartyID: p.PartyID,
		PayDate: p.Date, Mode: p.Mode, Amount: p.Amount, Reference: p.Reference, ChequeNo: p.ChequeNo,
		ChequeDate: p.ChequeDate, BankName: p.BankName, ChequeStatus: cheque, Notes: p.Notes, BillID: p.BillID,
		CreatedBy: *auth.UserID(ctx)})
	if err != nil {
		return 0, "", err
	}
	if p.PartyID != nil {
		narr := modeLabel(p.Mode) + " " + docNo
		if p.Reference != nil {
			narr += " (" + *p.Reference + ")"
		} else if p.ChequeNo != nil {
			narr += " (chq " + *p.ChequeNo + ")"
		}
		lp := dbq.InsertLedgerParams{PartyID: *p.PartyID, EntryDate: p.Date, RefType: kind, RefID: id, Narration: &narr}
		if p.Direction == dbq.PayDirectionReceived {
			lp.Credit = p.Amount
		} else {
			lp.Debit = p.Amount
		}
		if err := q.InsertLedger(ctx, lp); err != nil {
			return 0, "", err
		}
	}
	return id, docNo, nil
}

func modeLabel(m dbq.PayMode) string {
	switch m {
	case dbq.PayModeUpi:
		return "UPI"
	case dbq.PayModeBankTransfer:
		return "Bank transfer"
	case dbq.PayModeCheque:
		return "Cheque"
	}
	return "Cash"
}

// Allocate links (part of) a payment to a bill or purchase and refreshes
// the bill's amount paid.
func Allocate(ctx context.Context, q *dbq.Queries, paymentID int64, ref dbq.DocKind, refID int64, amount decimal.Decimal) error {
	if err := q.InsertAllocation(ctx, dbq.InsertAllocationParams{PaymentID: paymentID, RefType: ref, RefID: refID, Amount: amount}); err != nil {
		return err
	}
	if ref == dbq.DocKindCashBill || ref == dbq.DocKindAccountBill {
		if _, err := q.RefreshBillPaid(ctx, refID); err != nil {
			return err
		}
	}
	return nil
}

// Reverse undoes a payment's ledger effect (cheque bounce, cancel) and
// refreshes the bills it was allocated to.
func Reverse(ctx context.Context, q *dbq.Queries, p dbq.Payment, date time.Time) error {
	if p.PartyID != nil {
		if err := q.ReverseLedger(ctx, dbq.ReverseLedgerParams{EntryDate: date, RefType: docKind(p.Direction), RefID: p.ID}); err != nil {
			return err
		}
	}
	return refreshAllocated(ctx, q, p.ID)
}

func refreshAllocated(ctx context.Context, q *dbq.Queries, paymentID int64) error {
	bills, err := q.AllocatedBills(ctx, paymentID)
	if err != nil {
		return err
	}
	for _, b := range bills {
		if _, err := q.RefreshBillPaid(ctx, b); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------- responses

// LoadPayments renders payments with allocations, in order.
func LoadPayments(ctx context.Context, q *dbq.Queries, ids []int64) ([]api.Payment, error) {
	if len(ids) == 0 {
		return []api.Payment{}, nil
	}
	rows, err := q.PaymentsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	allocs, err := q.PaymentAllocations(ctx, ids)
	if err != nil {
		return nil, err
	}
	byAlloc := map[int64][]api.PaymentAllocation{}
	used := map[int64]decimal.Decimal{}
	for _, a := range allocs {
		byAlloc[a.PaymentID] = append(byAlloc[a.PaymentID], api.PaymentAllocation{RefType: string(a.RefType), RefID: a.RefID,
			RefNo: a.RefNo, Amount: api.M(a.Amount)})
		used[a.PaymentID] = used[a.PaymentID].Add(a.Amount)
	}
	byID := map[int64]api.Payment{}
	for _, p := range rows {
		x := api.Payment{ID: p.ID, DocNo: p.DocNo, Direction: string(p.Direction), PartyID: p.PartyID, PartyName: p.PartyName,
			PayDate: api.Date(p.PayDate), Mode: string(p.Mode), Amount: api.M(p.Amount), Reference: p.Reference,
			ChequeNo: p.ChequeNo, ChequeDate: api.DP(p.ChequeDate), BankName: p.BankName, Status: string(p.Status),
			Allocations: byAlloc[p.ID], Unallocated: api.M(p.Amount.Sub(used[p.ID]))}
		if p.ChequeStatus != nil {
			s := string(*p.ChequeStatus)
			x.ChequeStatus = &s
		}
		if x.Allocations == nil {
			x.Allocations = []api.PaymentAllocation{}
		}
		byID[p.ID] = x
	}
	out := make([]api.Payment, 0, len(ids))
	for _, id := range ids {
		if p, ok := byID[id]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func getPayment(ctx context.Context, q *dbq.Queries, id int64) (api.Payment, error) {
	ps, err := LoadPayments(ctx, q, []int64{id})
	if err != nil {
		return api.Payment{}, err
	}
	if len(ps) == 0 {
		return api.Payment{}, httpx.NotFound("payment")
	}
	return ps[0], nil
}

// ---------------------------------------------------------------- handlers

func (h handler) listPayments(w http.ResponseWriter, r *http.Request) error {
	page, err := httpx.ParsePage(r)
	if err != nil {
		return err
	}
	dir, err := httpx.QEnum(r, "direction", "received", "paid")
	if err != nil {
		return err
	}
	mode, err := httpx.QEnum(r, "mode", "cash", "upi", "cheque", "bank_transfer")
	if err != nil {
		return err
	}
	cheque, err := httpx.QEnum(r, "cheque_status", "pending", "cleared", "bounced")
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
	p := dbq.ListPaymentIDsParams{PartyID: party, FromDate: from, ToDate: to, Lim: page.Limit(), Off: page.Offset()}
	if dir != nil {
		d := dbq.PayDirection(*dir)
		p.Direction = &d
	}
	if mode != nil {
		m := dbq.PayMode(*mode)
		p.Mode = &m
	}
	if cheque != nil {
		c := dbq.ChequeState(*cheque)
		p.ChequeStatus = &c
	}
	rows, err := h.a.Q.ListPaymentIDs(r.Context(), p)
	if err != nil {
		return err
	}
	ids := make([]int64, len(rows))
	var total int64
	for i, row := range rows {
		ids[i], total = row.ID, row.Total
	}
	out, err := LoadPayments(r.Context(), h.a.Q, ids)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, httpx.NewList(out, page, total))
	return nil
}

func (h handler) getPayment(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	p, err := getPayment(r.Context(), h.a.Q, id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, p)
	return nil
}

// createPayment records a receipt or a dealer payment with optional
// allocations (FR-CU-03, FR-PU-03). Without allocations the money stays on
// account unless auto_allocate settles the oldest open documents first.
func (h handler) createPayment(w http.ResponseWriter, r *http.Request) error {
	body, err := httpx.ReadBody(r)
	if err != nil {
		return err
	}
	var in api.PaymentInput
	if err := httpx.DecodeBytes(body, &in); err != nil {
		return err
	}
	return idem.Run(w, r, h.a, body, func(ctx context.Context, _ pgx.Tx, q *dbq.Queries) (idem.Result, error) {
		parties, err := q.PartiesByIDs(ctx, []int64{in.PartyID})
		if err != nil {
			return idem.Result{}, err
		}
		if len(parties) == 0 || !parties[0].IsActive {
			return idem.Result{}, httpx.Validation("party_id", "unknown or inactive party")
		}
		party := parties[0]
		dir := dbq.PayDirection(in.Direction)
		if dir == dbq.PayDirectionReceived && party.Kind == dbq.PartyKindSupplier {
			return idem.Result{}, httpx.Validation("direction", "payments from a dealer-only party are not receipts; change the party kind to both")
		}
		if dir == dbq.PayDirectionPaid && party.Kind == dbq.PartyKindCustomer {
			return idem.Result{}, httpx.Validation("direction", "payments to a customer-only party; change the party kind to both")
		}
		amount := num.Parse(in.Amount)
		date := h.a.DateOr(httpx.ParseDate(in.PayDate))
		np := FromSplit(api.PaymentSplitInput{Mode: in.Mode, Amount: in.Amount, Reference: in.Reference,
			ChequeNo: in.ChequeNo, ChequeDate: in.ChequeDate, BankName: in.BankName}, dir, &in.PartyID, date)
		np.Notes = blank(in.Notes)

		allocs, err := h.planAllocations(ctx, q, in, party.ID, dir, amount)
		if err != nil {
			return idem.Result{}, err
		}
		id, _, err := Record(ctx, q, np)
		if err != nil {
			return idem.Result{}, err
		}
		for _, a := range allocs {
			if err := Allocate(ctx, q, id, a.ref, a.id, a.amount); err != nil {
				return idem.Result{}, err
			}
		}
		out, err := getPayment(ctx, q, id)
		if err != nil {
			return idem.Result{}, err
		}
		if err := audit.Log(ctx, q, "create", "payment", audit.ID(id), nil, out); err != nil {
			return idem.Result{}, err
		}
		return idem.Result{Status: http.StatusCreated, Body: out, Location: "/api/v1/payments/" + itoa(id)}, nil
	})
}

type allocation struct {
	ref    dbq.DocKind
	id     int64
	amount decimal.Decimal
}

func (h handler) planAllocations(ctx context.Context, q *dbq.Queries, in api.PaymentInput, partyID int64,
	dir dbq.PayDirection, amount decimal.Decimal) ([]allocation, error) {
	var out []allocation
	total := decimal.Zero
	if len(in.Allocations) > 0 {
		for i, a := range in.Allocations {
			f := fmt.Sprintf("allocations[%d]", i)
			amt := num.Parse(a.Amount)
			ref := dbq.DocKind(a.RefType)
			switch ref {
			case dbq.DocKindCashBill, dbq.DocKindAccountBill:
				if dir != dbq.PayDirectionReceived {
					return nil, httpx.Validation(f+".ref_type", "bills take receipts, not payments made")
				}
				b, err := q.LockBill(ctx, a.RefID)
				if err != nil || b.PartyID == nil || *b.PartyID != partyID {
					return nil, httpx.Validation(f+".ref_id", "bill not found for this party")
				}
				if (ref == dbq.DocKindCashBill) != (b.BillType == dbq.BillKindCash) {
					return nil, httpx.Validation(f+".ref_type", "does not match the bill type")
				}
				if b.Status != dbq.DocStatusActive || !b.PostsToLedger {
					return nil, httpx.Validation(f+".ref_id", "only active bills that post to the ledger take payments")
				}
				if b.BillType == dbq.BillKindAccount && in.Mode == "cash" {
					return nil, httpx.Validation("mode", "Account bills are paid by UPI, cheque or bank transfer (FR-AB-06)")
				}
				if amt.GreaterThan(num.Z(b.BalanceDue)) {
					return nil, httpx.Validation(f+".amount", "is more than the bill's balance due "+num.Z(b.BalanceDue).StringFixed(2))
				}
			case dbq.DocKindPurchase:
				if dir != dbq.PayDirectionPaid {
					return nil, httpx.Validation(f+".ref_type", "purchases take payments made, not receipts")
				}
				pu, err := q.LockPurchase(ctx, a.RefID)
				if err != nil || pu.SupplierID != partyID || pu.Status != dbq.DocStatusActive {
					return nil, httpx.Validation(f+".ref_id", "active purchase not found for this dealer")
				}
				paid, err := q.PurchasePaid(ctx, pu.ID)
				if err != nil {
					return nil, err
				}
				if amt.GreaterThan(pu.GrandTotal.Sub(paid)) {
					return nil, httpx.Validation(f+".amount", "is more than the purchase's balance "+pu.GrandTotal.Sub(paid).StringFixed(2))
				}
			}
			total = total.Add(amt)
			out = append(out, allocation{ref, a.RefID, amt})
		}
		if total.GreaterThan(amount) {
			return nil, httpx.Validation("allocations", "allocations add up to more than the payment amount")
		}
		return out, nil
	}
	if !in.AutoAllocate {
		return nil, nil // on account
	}
	left := amount
	if dir == dbq.PayDirectionReceived {
		bills, err := q.OpenBillsForParty(ctx, &partyID)
		if err != nil {
			return nil, err
		}
		for _, b := range bills {
			if !left.IsPositive() {
				break
			}
			if b.BillType == dbq.BillKindAccount && in.Mode == "cash" {
				continue // FR-AB-06
			}
			amt := decimal.Min(left, num.Z(b.BalanceDue))
			ref := dbq.DocKindCashBill
			if b.BillType == dbq.BillKindAccount {
				ref = dbq.DocKindAccountBill
			}
			out = append(out, allocation{ref, b.ID, amt})
			left = left.Sub(amt)
		}
		return out, nil
	}
	purchases, err := q.OpenPurchasesForParty(ctx, partyID)
	if err != nil {
		return nil, err
	}
	for _, pu := range purchases {
		if !left.IsPositive() {
			break
		}
		amt := decimal.Min(left, pu.Due)
		out = append(out, allocation{dbq.DocKindPurchase, pu.ID, amt})
		left = left.Sub(amt)
	}
	return out, nil
}

// chequeStatus marks a pending cheque cleared or bounced; a bounce reverses
// the receipt in the ledger and reopens the bills (FR-CU-03).
func (h handler) chequeStatus(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in api.ChequeStatusInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	var out api.Payment
	err = h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		p, err := q.LockPayment(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.NotFound("payment")
		}
		if err != nil {
			return err
		}
		if p.Mode != dbq.PayModeCheque || p.Status != dbq.DocStatusActive {
			return httpx.InvalidState("only active cheque payments have a cheque status")
		}
		if p.ChequeStatus == nil || *p.ChequeStatus != dbq.ChequeStatePending {
			return httpx.InvalidState("the cheque is already " + string(*p.ChequeStatus))
		}
		date := h.a.DateOr(httpx.ParseDate(in.Date))
		if in.Status == "cleared" {
			if err := q.SetChequeCleared(ctx, dbq.SetChequeClearedParams{ID: id, ClearedDate: &date}); err != nil {
				return err
			}
		} else {
			if err := q.SetChequeBounced(ctx, dbq.SetChequeBouncedParams{ID: id, BouncedDate: &date, Notes: blank(in.Notes)}); err != nil {
				return err
			}
			if err := Reverse(ctx, q, p, date); err != nil {
				return err
			}
		}
		if out, err = getPayment(ctx, q, id); err != nil {
			return err
		}
		return audit.Log(ctx, q, "cheque_"+in.Status, "payment", audit.ID(id), nil, out)
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, out)
	return nil
}

func (h handler) cancelPayment(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in api.ReasonInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	var out api.Payment
	err = h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		p, err := q.LockPayment(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.NotFound("payment")
		}
		if err != nil {
			return err
		}
		if p.Status != dbq.DocStatusActive {
			return httpx.InvalidState("payment is already cancelled")
		}
		before, err := getPayment(ctx, q, id)
		if err != nil {
			return err
		}
		if err := q.CancelPaymentRow(ctx, dbq.CancelPaymentRowParams{ID: id, Reason: &in.Reason, UserID: auth.UserID(ctx)}); err != nil {
			return err
		}
		// A bounced cheque was already reversed in the ledger.
		if p.ChequeStatus == nil || *p.ChequeStatus != dbq.ChequeStateBounced {
			if err := Reverse(ctx, q, p, h.a.Today()); err != nil {
				return err
			}
		} else if err := refreshAllocated(ctx, q, id); err != nil {
			return err
		}
		if out, err = getPayment(ctx, q, id); err != nil {
			return err
		}
		return audit.Log(ctx, q, "cancel", "payment", audit.ID(id), before, map[string]any{"payment": out, "reason": in.Reason})
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, out)
	return nil
}

func (h handler) receipt(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	p, err := getPayment(ctx, h.a.Q, id)
	if err != nil {
		return err
	}
	s, err := h.a.Q.GetSettings(ctx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	pdf, err := docs.Receipt(s, p)
	if err != nil {
		return err
	}
	link, err := docs.Publish(ctx, h.a, "receipts/"+strings.ReplaceAll(p.DocNo, "/", "-")+".pdf", pdf)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, link)
	return nil
}
