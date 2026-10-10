package einvoice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"billing-api/internal/app"
	"billing-api/internal/db/dbq"
)

const (
	RefBill       = "bill"
	RefCreditNote = "credit_note"
	maxAttempts   = 5
)

// GenerateArgs queues IRN generation for a bill or credit note (FR-EI-01/02).
type GenerateArgs struct {
	RefType string `json:"ref_type"`
	RefID   int64  `json:"ref_id"`
}

func (GenerateArgs) Kind() string { return "einvoice_generate" }
func (GenerateArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: maxAttempts}
}

// CancelArgs cancels an IRN inside the 24-hour window (FR-EI-03). With
// Regenerate (bill edited in the window) a new IRN is requested afterwards.
type CancelArgs struct {
	RefType    string `json:"ref_type"`
	RefID      int64  `json:"ref_id"`
	IRN        string `json:"irn"`
	Reason     string `json:"reason"`
	Regenerate bool   `json:"regenerate"`
}

func (CancelArgs) Kind() string { return "einvoice_cancel" }
func (CancelArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: maxAttempts}
}

// EWBArgs generates an e-way bill (FR-EI-04).
type EWBArgs struct {
	BillID        int64  `json:"bill_id"`
	VehicleNo     string `json:"vehicle_no"`
	TransportMode string `json:"transport_mode"`
	DistanceKm    int    `json:"distance_km"`
	TransporterID string `json:"transporter_id"`
}

func (EWBArgs) Kind() string { return "ewaybill_generate" }
func (EWBArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: maxAttempts}
}

// Register adds the e-invoice workers.
func Register(w *river.Workers, a *app.App) {
	river.AddWorker(w, &generateWorker{a: a})
	river.AddWorker(w, &cancelWorker{a: a})
	river.AddWorker(w, &ewbWorker{a: a})
}

type generateWorker struct {
	river.WorkerDefaults[GenerateArgs]
	a *app.App
}

func (w *generateWorker) Work(ctx context.Context, job *river.Job[GenerateArgs]) error {
	return generate(ctx, w.a, job.Args, job.Attempt >= job.MaxAttempts)
}

func generate(ctx context.Context, a *app.App, args GenerateArgs, lastAttempt bool) error {
	q := a.Q
	s, err := q.GetSettings(ctx)
	if err != nil {
		return err
	}
	var doc Document
	switch args.RefType {
	case RefBill:
		b, err := q.GetBill(ctx, args.RefID)
		if err != nil {
			return err
		}
		if b.EinvStatus == dbq.EinvStateGenerated || b.Status != dbq.DocStatusActive {
			return nil // already done, or cancelled meanwhile
		}
		if doc, err = BillDocument(ctx, q, s, b); err != nil {
			return err
		}
	case RefCreditNote:
		cn, err := q.GetCreditNote(ctx, args.RefID)
		if err != nil {
			return err
		}
		if cn.EinvStatus == dbq.EinvStateGenerated {
			return nil
		}
		if doc, err = CreditNoteDocument(ctx, q, s, cn); err != nil {
			return err
		}
	default:
		return river.JobCancel(fmt.Errorf("unknown ref type %q", args.RefType))
	}

	p, err := ProviderFor(a, s)
	var res IRNResult
	if err == nil {
		res, err = p.GenerateIRN(ctx, doc)
	}
	logCall(ctx, a, args.RefType, args.RefID, "generate_irn", doc, res, err)
	if err != nil {
		return fail(ctx, a, args.RefType, args.RefID, err, lastAttempt)
	}
	switch args.RefType {
	case RefBill:
		return q.SetBillIRN(ctx, dbq.SetBillIRNParams{ID: args.RefID, Irn: &res.IRN, AckNo: &res.AckNo,
			AckDate: &res.AckDate, SignedQr: &res.SignedQR})
	default:
		return q.SetCreditNoteIRN(ctx, dbq.SetCreditNoteIRNParams{ID: args.RefID, Irn: &res.IRN, AckNo: &res.AckNo,
			AckDate: &res.AckDate, SignedQr: &res.SignedQR})
	}
}

type cancelWorker struct {
	river.WorkerDefaults[CancelArgs]
	a *app.App
}

func (w *cancelWorker) Work(ctx context.Context, job *river.Job[CancelArgs]) error {
	a, args := w.a, job.Args
	s, err := a.Q.GetSettings(ctx)
	if err != nil {
		return err
	}
	p, err := ProviderFor(a, s)
	if err == nil {
		err = p.CancelIRN(ctx, args.IRN, args.Reason)
	}
	logCall(ctx, a, args.RefType, args.RefID, "cancel_irn", map[string]string{"Irn": args.IRN, "CnlRem": args.Reason}, nil, err)
	if err != nil {
		return fail(ctx, a, args.RefType, args.RefID, err, job.Attempt >= job.MaxAttempts)
	}
	if args.RefType == RefCreditNote {
		return a.Q.SetCreditNoteIRNCancelled(ctx, args.RefID)
	}
	if !args.Regenerate {
		return a.Q.SetBillIRNCancelled(ctx, args.RefID)
	}
	// Edited inside the window: clear the old IRN and request a new one.
	return a.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		if err := q.ClearBillIRN(ctx, args.RefID); err != nil {
			return err
		}
		_, err := a.River.InsertTx(ctx, tx, GenerateArgs{RefType: RefBill, RefID: args.RefID}, nil)
		return err
	})
}

type ewbWorker struct {
	river.WorkerDefaults[EWBArgs]
	a *app.App
}

func (w *ewbWorker) Work(ctx context.Context, job *river.Job[EWBArgs]) error {
	a, args := w.a, job.Args
	s, err := a.Q.GetSettings(ctx)
	if err != nil {
		return err
	}
	b, err := a.Q.GetBill(ctx, args.BillID)
	if err != nil {
		return err
	}
	req := EWBRequest{IRN: deref(b.Irn), DocNo: deref(b.BillNo), VehicleNo: args.VehicleNo,
		TransportMode: args.TransportMode, DistanceKm: args.DistanceKm, TransporterID: args.TransporterID}
	p, err := ProviderFor(a, s)
	var res EWBResult
	if err == nil {
		res, err = p.GenerateEWB(ctx, req)
	}
	logCall(ctx, a, RefBill, args.BillID, "generate_ewb", req, res, err)
	if err != nil {
		if job.Attempt >= job.MaxAttempts {
			msg := err.Error()
			_ = a.Q.SetBillEInvoiceError(ctx, dbq.SetBillEInvoiceErrorParams{ID: args.BillID, EinvStatus: b.EinvStatus, EinvError: &msg})
			notify(ctx, a, RefBill, args.BillID, "E-way bill failed for "+deref(b.BillNo), msg)
		}
		return err
	}
	until := res.ValidUntil
	return a.Q.SetBillEWB(ctx, dbq.SetBillEWBParams{ID: args.BillID, EwbNo: &res.EWBNo, EwbValidUntil: &until,
		VehicleNo: &args.VehicleNo})
}

// fail records the error; after the last attempt the document is marked
// failed and the Super Admin is notified (FR-EI-02).
func fail(ctx context.Context, a *app.App, refType string, refID int64, cause error, last bool) error {
	msg := cause.Error()
	status := dbq.EinvStatePending
	if last {
		status = dbq.EinvStateFailed
	}
	var err error
	if refType == RefBill {
		err = a.Q.SetBillEInvoiceError(ctx, dbq.SetBillEInvoiceErrorParams{ID: refID, EinvStatus: status, EinvError: &msg})
	} else {
		err = a.Q.SetCreditNoteEInvoiceError(ctx, dbq.SetCreditNoteEInvoiceErrorParams{ID: refID, EinvStatus: status, EinvError: &msg})
	}
	if err != nil {
		slog.ErrorContext(ctx, "record e-invoice failure", "err", err)
	}
	if last {
		notify(ctx, a, refType, refID, "E-invoice failed", msg)
	}
	var pe *PortalError
	if errors.As(cause, &pe) && last {
		return river.JobCancel(cause)
	}
	return cause
}

func notify(ctx context.Context, a *app.App, refType string, refID int64, title, body string) {
	role := dbq.UserRoleSuperAdmin
	rt := dbq.DocKindCashBill
	if refType == RefCreditNote {
		rt = dbq.DocKindCreditNote
	} else if b, err := a.Q.GetBill(ctx, refID); err == nil && b.BillType == dbq.BillKindAccount {
		rt = dbq.DocKindAccountBill
	}
	if err := a.Q.InsertNotification(ctx, dbq.InsertNotificationParams{
		ForRole: &role, Kind: "einv_failed", Title: title, Body: &body, RefType: &rt, RefID: &refID,
	}); err != nil {
		slog.ErrorContext(ctx, "e-invoice notification", "err", err)
	}
}

func logCall(ctx context.Context, a *app.App, refType string, refID int64, action string, req, resp any, callErr error) {
	rt := dbq.DocKindCashBill
	if refType == RefCreditNote {
		rt = dbq.DocKindCreditNote
	} else if b, err := a.Q.GetBill(ctx, refID); err == nil && b.BillType == dbq.BillKindAccount {
		rt = dbq.DocKindAccountBill
	}
	reqJSON, _ := json.Marshal(req)
	var respJSON []byte
	var code *string
	if callErr == nil {
		respJSON, _ = json.Marshal(resp)
	} else {
		respJSON, _ = json.Marshal(map[string]string{"error": callErr.Error()})
		c := "ERROR"
		var pe *PortalError
		if errors.As(callErr, &pe) {
			c = pe.Code
		}
		code = &c
	}
	if err := a.Q.InsertEInvoiceLog(ctx, dbq.InsertEInvoiceLogParams{
		RefType: rt, RefID: refID, Action: action, Request: reqJSON, Response: respJSON,
		Success: callErr == nil, ErrorCode: code,
	}); err != nil {
		slog.ErrorContext(ctx, "einvoice_log insert", "err", err)
	}
}

// CancelDeadline is IRN time + the edit window (24 h by default).
func CancelDeadline(ackDate *time.Time, windowHrs int32) *time.Time {
	if ackDate == nil {
		return nil
	}
	if windowHrs <= 0 {
		windowHrs = 24
	}
	t := ackDate.Add(time.Duration(windowHrs) * time.Hour)
	return &t
}
