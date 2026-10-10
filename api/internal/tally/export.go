// Package tally implements the ongoing Tally XML export (FR-TI-02..04) and
// the one-time import of masters and opening balances (FR-TI-01).
//
// Export is incremental: each document version is exported once
// (tally_export_item). A document exported before is sent again as an
// alteration; a cancelled bill, payment or purchase as a cancellation.
package tally

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/shopspring/decimal"

	"billing-api/internal/api"
	"billing-api/internal/app"
	"billing-api/internal/audit"
	"billing-api/internal/auth"
	"billing-api/internal/db/dbq"
	"billing-api/internal/httpx"
	"billing-api/internal/jobs"
)

type handler struct{ a *app.App }

func Routes(a *app.App) func(chi.Router) {
	h := handler{a}
	return func(r chi.Router) {
		r.With(auth.SuperAdmin).Post("/imports", httpx.H(h.preview))
		r.With(auth.SuperAdmin).Post("/imports/{id}/commit", httpx.H(h.commit))
		r.Post("/exports", httpx.H(h.export))
		r.Get("/syncs", httpx.H(h.syncs))
	}
}

func RegisterWorkers(w *river.Workers, a *app.App) {
	river.AddWorker(w, &exportWorker{a: a})
	river.AddWorker(w, &importWorker{a: a})
}

func (h handler) syncs(w http.ResponseWriter, r *http.Request) error {
	page, err := httpx.ParsePage(r)
	if err != nil {
		return err
	}
	rows, err := h.a.Q.ListTallySyncs(r.Context(), dbq.ListTallySyncsParams{Lim: page.Limit(), Off: page.Offset()})
	if err != nil {
		return err
	}
	out := make([]api.TallySync, len(rows))
	var total int64
	for i, s := range rows {
		total = s.Total
		var count int32
		if s.RecordCount != nil {
			count = *s.RecordCount
		}
		out[i] = api.TallySync{ID: s.ID, Direction: s.Direction, Scope: s.Scope, PeriodFrom: api.DP(s.PeriodFrom),
			PeriodTo: api.DP(s.PeriodTo), RecordCount: count, Status: s.Status, CreatedByName: s.CreatedByName, CreatedAt: s.CreatedAt}
	}
	httpx.JSON(w, http.StatusOK, httpx.NewList(out, page, total))
	return nil
}

// ---------------------------------------------------------------- export request

type exportParams struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Scope  []string  `json:"scope"`
	All    bool      `json:"include_already_exported"`
	UserID uuid.UUID `json:"user_id"`
}

type ExportArgs struct {
	JobID uuid.UUID `json:"job_id"`
}

func (ExportArgs) Kind() string { return "tally_export" }

func (h handler) export(w http.ResponseWriter, r *http.Request) error {
	var in api.TallyExportInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	from, to := *httpx.ParseDate(&in.From), *httpx.ParseDate(&in.To)
	if to.Before(from) {
		return httpx.Validation("to", "must not be before from")
	}
	ctx := r.Context()
	var job dbq.Job
	err := h.a.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		var err error
		job, err = jobs.Create(ctx, q, "tally_export", exportParams{From: from, To: to, Scope: in.Scope,
			All: in.IncludeAlreadyExported, UserID: auth.MustUser(r).ID})
		if err != nil {
			return err
		}
		_, err = h.a.River.InsertTx(ctx, tx, ExportArgs{JobID: job.ID}, nil)
		return err
	})
	if err != nil {
		return err
	}
	jobs.Accepted(w, r, h.a, job)
	return nil
}

// ---------------------------------------------------------------- XML model

type envelope struct {
	XMLName xml.Name `xml:"ENVELOPE"`
	Header  struct {
		TallyRequest string `xml:"TALLYREQUEST"`
	} `xml:"HEADER"`
	Body struct {
		ImportData struct {
			RequestDesc struct {
				ReportName string `xml:"REPORTNAME"`
			} `xml:"REQUESTDESC"`
			RequestData struct {
				Messages []message `xml:"TALLYMESSAGE"`
			} `xml:"REQUESTDATA"`
		} `xml:"IMPORTDATA"`
	} `xml:"BODY"`
}

type message struct {
	UDF        string      `xml:"xmlns:UDF,attr"`
	StockGroup *stockGroup `xml:"STOCKGROUP,omitempty"`
	Unit       *unitMaster `xml:"UNIT,omitempty"`
	StockItem  *stockItem  `xml:"STOCKITEM,omitempty"`
	Ledger     *ledgerM    `xml:"LEDGER,omitempty"`
	Voucher    *voucher    `xml:"VOUCHER,omitempty"`
}

type nameList struct {
	Name string `xml:"NAME"`
}

type stockGroup struct {
	Name     string   `xml:"NAME,attr"`
	NameList nameList `xml:"NAME.LIST"`
}

type unitMaster struct {
	Name         string `xml:"NAME,attr"`
	UnitName     string `xml:"NAME"`
	IsSimpleUnit string `xml:"ISSIMPLEUNIT"`
}

type stockItem struct {
	Name      string   `xml:"NAME,attr"`
	NameList  nameList `xml:"NAME.LIST"`
	Parent    string   `xml:"PARENT"`
	BaseUnits string   `xml:"BASEUNITS"`
	GSTApp    string   `xml:"GSTAPPLICABLE"`
	HSN       string   `xml:"GSTDETAILS.LIST>HSNCODE,omitempty"`
}

type ledgerM struct {
	Name     string   `xml:"NAME,attr"`
	NameList nameList `xml:"NAME.LIST"`
	Parent   string   `xml:"PARENT"`
	GSTIN    string   `xml:"PARTYGSTIN,omitempty"`
	State    string   `xml:"LEDSTATENAME,omitempty"`
	Mobile   string   `xml:"LEDGERMOBILE,omitempty"`
	Address  []string `xml:"ADDRESS.LIST>ADDRESS,omitempty"`
	Opening  string   `xml:"OPENINGBALANCE"`
}

type voucher struct {
	RemoteID      string  `xml:"REMOTEID,attr"`
	VchType       string  `xml:"VCHTYPE,attr"`
	Action        string  `xml:"ACTION,attr"`
	Date          string  `xml:"DATE"`
	VoucherType   string  `xml:"VOUCHERTYPENAME"`
	VoucherNumber string  `xml:"VOUCHERNUMBER"`
	Reference     string  `xml:"REFERENCE,omitempty"`
	PartyLedger   string  `xml:"PARTYLEDGERNAME,omitempty"`
	Narration     string  `xml:"NARRATION,omitempty"`
	IsCancelled   string  `xml:"ISCANCELLED,omitempty"`
	PersistedView string  `xml:"PERSISTEDVIEW"`
	Entries       []entry `xml:"ALLLEDGERENTRIES.LIST"`
}

type entry struct {
	LedgerName       string `xml:"LEDGERNAME"`
	IsDeemedPositive string `xml:"ISDEEMEDPOSITIVE"`
	Amount           string `xml:"AMOUNT"`
}

// Tally sign: debits are negative with ISDEEMEDPOSITIVE=Yes.
func dr(ledger string, amt decimal.Decimal) entry {
	return entry{LedgerName: ledger, IsDeemedPositive: "Yes", Amount: amt.Neg().StringFixed(2)}
}
func cr(ledger string, amt decimal.Decimal) entry {
	return entry{LedgerName: ledger, IsDeemedPositive: "No", Amount: amt.StringFixed(2)}
}

// mapping with defaults for unset ledgers.
type mapping struct{ api.TallyMapping }

func or(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func loadMapping(s dbq.ShopSetting) mapping {
	var m api.TallyMapping
	_ = json.Unmarshal(s.TallyMapping, &m)
	m.SalesLedgerCash = or(m.SalesLedgerCash, "Sales")
	m.SalesLedgerAccount = or(m.SalesLedgerAccount, "Sales")
	m.CGSTLedger = or(m.CGSTLedger, "CGST")
	m.SGSTLedger = or(m.SGSTLedger, "SGST")
	m.IGSTLedger = or(m.IGSTLedger, "IGST")
	m.LabourLedger = or(m.LabourLedger, "Labour Charges")
	m.TransportLedger = or(m.TransportLedger, "Transport Charges")
	m.RoundOffLedger = or(m.RoundOffLedger, "Round Off")
	m.CashLedger = or(m.CashLedger, "Cash")
	m.BankLedger = or(m.BankLedger, "Bank")
	m.PurchaseLedger = or(m.PurchaseLedger, "Purchase")
	m.VoucherTypeCash = or(m.VoucherTypeCash, "Sales")
	m.VoucherTypeAccount = or(m.VoucherTypeAccount, "Sales")
	return mapping{m}
}

func tallyDate(t time.Time) string { return t.Format("20060102") }

// action: first export = Create; later version = Alter; cancelled = Cancel.
func action(exportedBefore, cancelled bool) (string, string) {
	switch {
	case cancelled && exportedBefore:
		return "Cancel", ""
	case cancelled:
		return "Create", "Yes"
	case exportedBefore:
		return "Alter", ""
	}
	return "Create", ""
}

func addTaxes(es []entry, m mapping, credit bool, cgst, sgst, igst decimal.Decimal) []entry {
	put := func(ledger string, amt decimal.Decimal) {
		if amt.IsZero() {
			return
		}
		if credit {
			es = append(es, cr(ledger, amt))
		} else {
			es = append(es, dr(ledger, amt))
		}
	}
	put(m.CGSTLedger, cgst)
	put(m.SGSTLedger, sgst)
	put(m.IGSTLedger, igst)
	return es
}

func roundOff(es []entry, m mapping, amt decimal.Decimal, creditWhenPositive bool) []entry {
	switch {
	case amt.IsZero():
		return es
	case amt.IsPositive() == creditWhenPositive:
		return append(es, cr(m.RoundOffLedger, amt.Abs()))
	default:
		return append(es, dr(m.RoundOffLedger, amt.Abs()))
	}
}

type exported struct {
	ref     dbq.DocKind
	id      int64
	version int32
}

// build assembles the XML for one export run.
func build(ctx context.Context, q *dbq.Queries, s dbq.ShopSetting, p exportParams) ([]byte, []exported, map[string]int, error) {
	m := loadMapping(s)
	var env envelope
	env.Header.TallyRequest = "Import Data"
	env.Body.ImportData.RequestDesc.ReportName = "All Masters"
	msgs := &env.Body.ImportData.RequestData.Messages
	add := func(msg message) { msg.UDF = "TallyUDF"; *msgs = append(*msgs, msg) }
	counts := map[string]int{}
	var done []exported
	has := func(s string) bool {
		for _, x := range p.Scope {
			if x == s {
				return true
			}
		}
		return false
	}

	if has("masters") {
		items, err := q.TallyItems(ctx)
		if err != nil {
			return nil, nil, nil, err
		}
		groups, units := map[string]bool{}, map[string]bool{}
		for _, it := range items {
			if !groups[it.Grp] {
				groups[it.Grp] = true
				add(message{StockGroup: &stockGroup{Name: it.Grp, NameList: nameList{it.Grp}}})
			}
			if !units[it.Unit] {
				units[it.Unit] = true
				add(message{Unit: &unitMaster{Name: it.Unit, UnitName: it.Unit, IsSimpleUnit: "Yes"}})
			}
			add(message{StockItem: &stockItem{Name: it.Name, NameList: nameList{it.Name}, Parent: it.Grp, BaseUnits: it.Unit,
				GSTApp: "Applicable", HSN: it.HsnCode}})
			counts["items"]++
		}
		parties, err := q.TallyParties(ctx)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, pt := range parties {
			parent := "Sundry Debtors"
			if pt.Kind == dbq.PartyKindSupplier {
				parent = "Sundry Creditors"
			}
			l := &ledgerM{Name: pt.Ledger, NameList: nameList{pt.Ledger}, Parent: parent, GSTIN: strings.TrimSpace(deref(pt.Gstin)),
				State: stateName(deref(pt.StateCode)), Mobile: deref(pt.Mobile), Opening: pt.OpeningBalance.Neg().StringFixed(2)}
			if pt.BillingAddress != nil {
				l.Address = []string{*pt.BillingAddress}
			}
			add(message{Ledger: l})
			counts["ledgers"]++
		}
	}

	if has("sales") {
		bills, err := q.TallyBills(ctx, dbq.TallyBillsParams{FromDate: p.From, ToDate: p.To, IncludeAll: p.All})
		if err != nil {
			return nil, nil, nil, err
		}
		ids := make([]int64, len(bills))
		for i, b := range bills {
			ids[i] = b.ID
		}
		charges, err := q.TallyBillCharges(ctx, ids)
		if err != nil {
			return nil, nil, nil, err
		}
		byBill := map[int64]map[dbq.ChargeKind]decimal.Decimal{}
		for _, c := range charges {
			if byBill[c.BillID] == nil {
				byBill[c.BillID] = map[dbq.ChargeKind]decimal.Decimal{}
			}
			byBill[c.BillID][c.Kind] = c.Amount
		}
		for _, b := range bills {
			act, isCancelled := action(b.ExportedBefore, b.Status == dbq.DocStatusCancelled)
			vtype, sales, ref := m.VoucherTypeCash, m.SalesLedgerCash, dbq.DocKindCashBill
			if b.BillType == dbq.BillKindAccount {
				vtype, sales, ref = m.VoucherTypeAccount, m.SalesLedgerAccount, dbq.DocKindAccountBill
			}
			party := b.PartyLedger
			if b.PartyID == nil {
				party = m.CashLedger
			}
			es := []entry{dr(party, b.GrandTotal), cr(sales, b.ItemsTaxable.Add(byBill[b.ID][dbq.ChargeKindOther]))}
			if v := byBill[b.ID][dbq.ChargeKindLabour]; !v.IsZero() {
				es = append(es, cr(m.LabourLedger, v))
			}
			if v := byBill[b.ID][dbq.ChargeKindTransport]; !v.IsZero() {
				es = append(es, cr(m.TransportLedger, v))
			}
			es = addTaxes(es, m, true, b.Cgst, b.Sgst, b.Igst)
			es = roundOff(es, m, b.RoundOff, true)
			add(message{Voucher: &voucher{RemoteID: fmt.Sprintf("billing-bill-%d", b.ID), VchType: vtype, Action: act,
				Date: tallyDate(b.BillDate), VoucherType: vtype, VoucherNumber: deref(b.BillNo), PartyLedger: party,
				Narration: b.CustName, IsCancelled: isCancelled, PersistedView: "Accounting Voucher View", Entries: es}})
			done = append(done, exported{ref, b.ID, b.Version})
			counts["sales"]++
		}
	}

	if has("credit_notes") {
		cns, err := q.TallyCreditNotes(ctx, dbq.TallyCreditNotesParams{FromDate: p.From, ToDate: p.To, IncludeAll: p.All})
		if err != nil {
			return nil, nil, nil, err
		}
		for _, c := range cns {
			party := c.PartyLedger
			if c.PartyID == nil {
				party = m.CashLedger
			}
			sales := m.SalesLedgerCash
			if c.BillType == dbq.BillKindAccount {
				sales = m.SalesLedgerAccount
			}
			es := []entry{cr(party, c.GrandTotal), dr(sales, c.TaxableTotal)}
			es = addTaxes(es, m, false, c.Cgst, c.Sgst, c.Igst)
			add(message{Voucher: &voucher{RemoteID: fmt.Sprintf("billing-cn-%d", c.ID), VchType: "Credit Note", Action: "Create",
				Date: tallyDate(c.CnDate), VoucherType: "Credit Note", VoucherNumber: deref(c.CnNo), PartyLedger: party,
				PersistedView: "Accounting Voucher View", Entries: es}})
			done = append(done, exported{dbq.DocKindCreditNote, c.ID, 1})
			counts["credit_notes"]++
		}
	}

	for _, scope := range []struct {
		key string
		dir dbq.PayDirection
	}{{"receipts", dbq.PayDirectionReceived}, {"payments", dbq.PayDirectionPaid}} {
		if !has(scope.key) {
			continue
		}
		dir := scope.dir
		pays, err := q.TallyPayments(ctx, dbq.TallyPaymentsParams{FromDate: p.From, ToDate: p.To, Direction: &dir, IncludeAll: p.All})
		if err != nil {
			return nil, nil, nil, err
		}
		for _, py := range pays {
			act, isCancelled := action(py.ExportedBefore, py.Version == 2)
			money := m.BankLedger
			if py.Mode == dbq.PayModeCash {
				money = m.CashLedger
			}
			party := py.PartyLedger
			if py.PartyID == nil {
				party = m.CashLedger
			}
			vtype, ref, es := "Receipt", dbq.DocKindReceipt, []entry{dr(money, py.Amount), cr(party, py.Amount)}
			if py.Direction == dbq.PayDirectionPaid {
				vtype, ref, es = "Payment", dbq.DocKindPaymentVoucher, []entry{dr(party, py.Amount), cr(money, py.Amount)}
			}
			add(message{Voucher: &voucher{RemoteID: fmt.Sprintf("billing-pay-%d", py.ID), VchType: vtype, Action: act,
				Date: tallyDate(py.PayDate), VoucherType: vtype, VoucherNumber: py.DocNo, PartyLedger: party,
				Narration: py.Reference, IsCancelled: isCancelled, PersistedView: "Accounting Voucher View", Entries: es}})
			done = append(done, exported{ref, py.ID, py.Version})
			counts[scope.key]++
		}
	}

	if has("purchases") {
		pus, err := q.TallyPurchases(ctx, dbq.TallyPurchasesParams{FromDate: p.From, ToDate: p.To, IncludeAll: p.All})
		if err != nil {
			return nil, nil, nil, err
		}
		for _, pu := range pus {
			act, isCancelled := action(pu.ExportedBefore, pu.Version == 2)
			es := []entry{cr(pu.PartyLedger, pu.GrandTotal), dr(m.PurchaseLedger, pu.TaxableTotal.Add(pu.Freight).Add(pu.OtherCharges))}
			es = addTaxes(es, m, false, pu.Cgst, pu.Sgst, pu.Igst)
			es = roundOff(es, m, pu.RoundOff, false)
			add(message{Voucher: &voucher{RemoteID: fmt.Sprintf("billing-purchase-%d", pu.ID), VchType: "Purchase", Action: act,
				Date: tallyDate(pu.ReceivedDate), VoucherType: "Purchase", VoucherNumber: pu.DocNo,
				Reference: pu.SupplierInvoiceNo, PartyLedger: pu.PartyLedger, IsCancelled: isCancelled,
				PersistedView: "Accounting Voucher View", Entries: es}})
			done = append(done, exported{dbq.DocKindPurchase, pu.ID, pu.Version})
			counts["purchases"]++
		}
	}

	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := enc.Encode(env); err != nil {
		return nil, nil, nil, err
	}
	return buf.Bytes(), done, counts, nil
}

type exportWorker struct {
	river.WorkerDefaults[ExportArgs]
	a *app.App
}

func (wk *exportWorker) Work(ctx context.Context, job *river.Job[ExportArgs]) error {
	a := wk.a
	return jobs.Track(ctx, a, job.Args.JobID, func(ctx context.Context) (jobs.Outcome, error) {
		j, err := a.Q.GetJob(ctx, job.Args.JobID)
		if err != nil {
			return jobs.Outcome{}, err
		}
		var p exportParams
		if err := json.Unmarshal(j.Params, &p); err != nil {
			return jobs.Outcome{}, err
		}
		if u, err := a.Q.GetAppUser(ctx, p.UserID); err == nil {
			ctx = auth.WithUser(ctx, auth.User{ID: u.ID, FullName: u.FullName, Role: string(u.Role), Row: u})
		}
		var out jobs.Outcome
		err = a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
			s, err := q.GetSettings(ctx)
			if err != nil {
				return fmt.Errorf("shop settings are not set up: %w", err)
			}
			xmlData, done, counts, err := build(ctx, q, s, p)
			if err != nil {
				return err
			}
			path := fmt.Sprintf("tally/exports/%s-%s-%s.xml", p.From.Format("20060102"), p.To.Format("20060102"), job.Args.JobID.String()[:8])
			if err := a.Storage.Upload(ctx, path, "application/xml", xmlData); err != nil {
				return err
			}
			summary, _ := json.Marshal(counts)
			from, to := p.From, p.To
			syncID, err := q.InsertTallySync(ctx, dbq.InsertTallySyncParams{Direction: "export", Scope: strings.Join(p.Scope, ","),
				PeriodFrom: &from, PeriodTo: &to, FilePath: &path, RecordCount: ptr(int32(len(done))), Summary: summary,
				Status: "completed", CreatedBy: auth.UserID(ctx)})
			if err != nil {
				return err
			}
			for _, d := range done {
				if err := q.InsertTallyExportItem(ctx, dbq.InsertTallyExportItemParams{SyncID: syncID, RefType: d.ref,
					RefID: d.id, DocVersion: d.version}); err != nil {
					return err
				}
			}
			out = jobs.Outcome{Result: map[string]any{"sync_id": syncID, "counts": counts, "documents": len(done)}, Path: path}
			return audit.Log(ctx, q, "export", "tally", audit.ID(syncID), nil, map[string]any{"scope": p.Scope, "counts": counts})
		})
		return out, err
	})
}

func ptr[T any](v T) *T { return &v }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}
