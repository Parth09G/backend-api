// Package report builds the dashboard (FR-DB) and the reports with their
// Excel / PDF exports (section 3.12).
package report

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"billing-api/internal/api"
	"billing-api/internal/app"
	"billing-api/internal/db/dbq"
	"billing-api/internal/httpx"
	"billing-api/internal/inventory"
	"billing-api/internal/num"
)

type handler struct{ a *app.App }

func DashboardRoutes(a *app.App) func(chi.Router) {
	h := handler{a}
	return func(r chi.Router) {
		r.Get("/summary", httpx.H(h.summary))
		r.Get("/sales-trend", httpx.H(h.trend))
		r.Get("/categories", httpx.H(h.categories))
		r.Get("/top-items", httpx.H(h.topItems))
		r.Get("/low-stock", httpx.H(h.lowStock))
		r.Get("/pending", httpx.H(h.pending))
	}
}

// fyStart is 1 April of the financial year containing d.
func fyStart(d time.Time) time.Time {
	y := d.Year()
	if d.Month() < time.April {
		y--
	}
	return time.Date(y, time.April, 1, 0, 0, 0, 0, time.UTC)
}

// period reads ?from&to; defaults to the current financial year to date.
func (h handler) period(r *http.Request) (time.Time, time.Time, error) {
	from, to, err := httpx.Range(r)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	today := h.a.Today()
	f, t := fyStart(today), today
	if from != nil {
		f = *from
	}
	if to != nil {
		t = *to
	}
	return f, t, nil
}

func (h handler) summary(w http.ResponseWriter, r *http.Request) error {
	from, to, err := h.period(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	q := h.a.Q
	today := h.a.Today()
	fy, err := q.FY(ctx, today)
	if err != nil {
		return err
	}
	st, err := q.DashStock(ctx)
	if err != nil {
		return err
	}
	byLoc, err := q.DashStockByLocation(ctx)
	if err != nil {
		return err
	}
	sales, err := q.DashSales(ctx, dbq.DashSalesParams{Today: today, FromDate: from, ToDate: to, Fy: fy})
	if err != nil {
		return err
	}
	bal, err := q.DashBalances(ctx)
	if err != nil {
		return err
	}
	cheques, err := q.DashChequesPending(ctx)
	if err != nil {
		return err
	}
	coll, err := q.DashCollections(ctx, dbq.DashCollectionsParams{FromDate: from, ToDate: to})
	if err != nil {
		return err
	}
	out := api.DashboardSummary{
		Period: api.Period{From: api.Date(from), To: api.Date(to)},
		Stock: api.StockSummary{Items: st.Items, Value: api.M(st.Value), LowCount: st.LowCount, OutCount: st.OutCount,
			ByLocation: []api.LocationValue{}},
		Sales: api.SalesSummary{Today: api.M(sales.Today), Period: api.M(sales.Period), FY: api.M(sales.FyTotal),
			CashBills: api.M(sales.CashBills), AccountBills: api.M(sales.AccountBills), BillCount: sales.BillCount},
		GrossProfit: api.M(sales.Profit), GrossMarginPct: "0",
		Receivables: api.M(bal.Receivables), Payables: api.M(bal.Payables), ChequesPending: api.M(cheques),
		CollectionsByMode: []api.ModeAmount{},
	}
	if sales.ItemsTaxable.IsPositive() {
		out.GrossMarginPct = num.Trim(sales.Profit.Mul(num.Hundred).Div(sales.ItemsTaxable).Round(2))
	}
	for _, l := range byLoc {
		out.Stock.ByLocation = append(out.Stock.ByLocation, api.LocationValue{LocationID: int64(l.LocationID), Name: l.Name, Value: api.M(l.Value)})
	}
	for _, c := range coll {
		out.CollectionsByMode = append(out.CollectionsByMode, api.ModeAmount{Mode: c.Mode, Amount: api.M(c.Amount)})
	}
	httpx.JSON(w, http.StatusOK, out)
	return nil
}

func (h handler) trend(w http.ResponseWriter, r *http.Request) error {
	from, to, err := h.period(r)
	if err != nil {
		return err
	}
	bucket, err := httpx.QEnum(r, "bucket", "day", "month")
	if err != nil {
		return err
	}
	bt, err := httpx.QEnum(r, "bill_type", "cash", "account")
	if err != nil {
		return err
	}
	b := "day"
	if bucket != nil {
		b = *bucket
	} else if to.Sub(from) > 62*24*time.Hour {
		b = "month"
	}
	p := dbq.DashTrendParams{Bucket: b, FromDate: from, ToDate: to}
	if bt != nil {
		k := dbq.BillKind(*bt)
		p.BillType = &k
	}
	rows, err := h.a.Q.DashTrend(r.Context(), p)
	if err != nil {
		return err
	}
	out := make([]api.TrendPoint, len(rows))
	for i, t := range rows {
		out[i] = api.TrendPoint{Bucket: t.Bucket, Sales: api.M(t.Sales), Profit: api.M(t.Profit), Bills: t.Bills}
	}
	httpx.JSON(w, http.StatusOK, out)
	return nil
}

func (h handler) categories(w http.ResponseWriter, r *http.Request) error {
	from, to, err := h.period(r)
	if err != nil {
		return err
	}
	loc, err := httpx.QInt64(r, "location_id")
	if err != nil {
		return err
	}
	p := dbq.DashCategoriesParams{FromDate: from, ToDate: to}
	if loc != nil {
		l := int16(*loc)
		p.LocationID = &l
	}
	rows, err := h.a.Q.DashCategories(r.Context(), p)
	if err != nil {
		return err
	}
	out := make([]api.CategoryBreakdown, len(rows))
	for i, c := range rows {
		out[i] = api.CategoryBreakdown{CategoryID: int64(c.CategoryID), Name: c.Name, StockQtyItems: c.StockItems,
			StockValue: api.M(c.StockValue), Sales: api.M(c.Sales), Profit: api.M(c.Profit)}
	}
	httpx.JSON(w, http.StatusOK, out)
	return nil
}

func (h handler) topItems(w http.ResponseWriter, r *http.Request) error {
	from, to, err := h.period(r)
	if err != nil {
		return err
	}
	kind, err := httpx.QEnum(r, "kind", "fast", "slow")
	if err != nil {
		return err
	}
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 50 {
			return httpx.Validation("limit", "must be between 1 and 50")
		}
		limit = n
	}
	rows, err := h.a.Q.DashTopItems(r.Context(), dbq.DashTopItemsParams{FromDate: from, ToDate: to,
		Slow: kind != nil && *kind == "slow", Lim: int32(limit)})
	if err != nil {
		return err
	}
	out := make([]api.TopItem, len(rows))
	for i, t := range rows {
		out[i] = api.TopItem{ItemID: t.ItemID, Name: t.Name, QtySold: api.Q(t.QtySold), Unit: t.Unit, Sales: api.M(t.Sales)}
	}
	httpx.JSON(w, http.StatusOK, out)
	return nil
}

func (h handler) lowStock(w http.ResponseWriter, r *http.Request) error {
	loc, err := httpx.QInt64(r, "location_id")
	if err != nil {
		return err
	}
	var l *int16
	if loc != nil {
		v := int16(*loc)
		l = &v
	}
	ids, err := h.a.Q.DashLowStockIDs(r.Context(), l)
	if err != nil {
		return err
	}
	items, err := inventory.LoadItems(r.Context(), h.a.Q, ids)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, items)
	return nil
}

func (h handler) pending(w http.ResponseWriter, r *http.Request) error {
	p, err := h.a.Q.DashPending(r.Context(), h.a.Today())
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, api.PendingWork{EInvoiceFailed: p.EinvoiceFailed, EInvoicePending: p.EinvoicePending,
		ReturnsPending: p.ReturnsPending, BlockedBillsToday: p.BlockedBillsToday, TallyUnexported: p.TallyUnexported,
		ChequesDue: p.ChequesDue})
	return nil
}
