package inventory

import (
	"context"
	"net/http"
	"sort"
	"time"

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
	"billing-api/internal/num"
)

// TransferRoutes mounts /stock-transfers.
func TransferRoutes(a *app.App) func(chi.Router) {
	h := handler{a}
	return func(r chi.Router) {
		r.Get("/", httpx.H(h.listTransfers))
		r.Post("/", httpx.H(h.createTransfer))
		r.Get("/{id}", httpx.H(h.getTransfer))
	}
}

// AdjustmentRoutes mounts /stock-adjustments.
func AdjustmentRoutes(a *app.App) func(chi.Router) {
	h := handler{a}
	return func(r chi.Router) {
		r.Get("/", httpx.H(h.listAdjustments))
		r.Post("/", httpx.H(h.createAdjustment))
	}
}

// CountRoutes mounts /stock-counts.
func CountRoutes(a *app.App) func(chi.Router) {
	h := handler{a}
	return func(r chi.Router) {
		r.Post("/preview", httpx.H(h.previewCount))
		r.Post("/", httpx.H(h.postCount))
	}
}

// ---------------------------------------------------------------- shortage check

// Need is the base quantity a document line takes from a location.
type Need struct {
	LineNo     int
	ItemID     int64
	LocationID int16
	Qty        decimal.Decimal
}

// CheckShortages compares needs (summed per item and location) with stock.
// With lock=true the stock rows are locked FOR UPDATE in a fixed order, so
// concurrent documents cannot oversell (NFR-02). Returns the short lines
// with stock available at other locations (FR-CB-02).
func CheckShortages(ctx context.Context, q *dbq.Queries, needs []Need, lock bool) ([]api.ShortLine, error) {
	if len(needs) == 0 {
		return nil, nil
	}
	type key struct {
		item int64
		loc  int16
	}
	sum := map[key]decimal.Decimal{}
	first := map[key]int{}
	itemSet := map[int64]bool{}
	var order []key
	for _, n := range needs {
		k := key{n.ItemID, n.LocationID}
		if _, ok := sum[k]; !ok {
			order = append(order, k)
			first[k] = n.LineNo
		}
		sum[k] = sum[k].Add(n.Qty)
		itemSet[n.ItemID] = true
	}
	ids := make([]int64, 0, len(itemSet))
	for id := range itemSet {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	type bal struct {
		loc int16
		qty decimal.Decimal
	}
	have := map[key]decimal.Decimal{}
	byItem := map[int64][]bal{}
	add := func(item int64, loc int16, qty decimal.Decimal) {
		have[key{item, loc}] = qty
		byItem[item] = append(byItem[item], bal{loc, qty})
	}
	if lock {
		rows, err := q.LockStockRows(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			add(r.ItemID, r.LocationID, r.Qty)
		}
	} else {
		rows, err := q.StockRows(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			add(r.ItemID, r.LocationID, r.Qty)
		}
	}
	var short []api.ShortLine
	var names map[int64]dbq.ItemsByIDsRow
	var locNames map[int16]string
	for _, k := range order {
		if have[k].GreaterThanOrEqual(sum[k]) {
			continue
		}
		if names == nil {
			var err error
			if names, locNames, err = lookupNames(ctx, q, ids); err != nil {
				return nil, err
			}
		}
		sl := api.ShortLine{LineNo: first[k], ItemID: k.item, ItemName: names[k.item].Name, LocationID: int64(k.loc),
			LocationName: locNames[k.loc], Required: api.Q(sum[k]), Available: api.Q(have[k]),
			Unit: names[k.item].UnitSymbol, AvailableElsewhere: []api.ShortLineElsewhere{}}
		for _, b := range byItem[k.item] {
			if b.loc != k.loc && b.qty.IsPositive() {
				sl.AvailableElsewhere = append(sl.AvailableElsewhere, api.ShortLineElsewhere{LocationID: int64(b.loc),
					LocationName: locNames[b.loc], Qty: api.Q(b.qty)})
			}
		}
		short = append(short, sl)
	}
	return short, nil
}

func lookupNames(ctx context.Context, q *dbq.Queries, ids []int64) (map[int64]dbq.ItemsByIDsRow, map[int16]string, error) {
	rows, err := q.ItemsByIDs(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	items := map[int64]dbq.ItemsByIDsRow{}
	for _, r := range rows {
		items[r.ID] = r
	}
	locs, err := q.ListLocations(ctx)
	if err != nil {
		return nil, nil, err
	}
	names := map[int16]string{}
	for _, l := range locs {
		names[l.ID] = l.Name
	}
	return items, names, nil
}

// ShortageError is returned inside a transaction to roll it back; the
// handler turns it into 422 INSUFFICIENT_STOCK.
type ShortageError struct{ Lines []api.ShortLine }

func (e *ShortageError) Error() string { return "INSUFFICIENT_STOCK" }

// ShortageProblem renders the 422 response.
func ShortageProblem(lines []api.ShortLine, alertSent bool) *httpx.Problem {
	return &httpx.Problem{Status: http.StatusUnprocessableEntity, Code: httpx.CodeInsufficient,
		Detail: "stock is short at the chosen location", Extra: map[string]any{"short_lines": lines, "alert_sent": alertSent}}
}

type unitInfo struct {
	Factor   decimal.Decimal
	Symbol   string
	Decimals int16
}

// UnitFactors loads item unit factors keyed by item, then unit.
func UnitFactors(ctx context.Context, q *dbq.Queries, itemIDs []int64) (map[int64]map[int32]unitInfo, error) {
	rows, err := q.ItemUnitsByItems(ctx, itemIDs)
	if err != nil {
		return nil, err
	}
	out := map[int64]map[int32]unitInfo{}
	for _, r := range rows {
		if out[r.ItemID] == nil {
			out[r.ItemID] = map[int32]unitInfo{}
		}
		out[r.ItemID][r.UnitID] = unitInfo{Factor: r.FactorToBase, Symbol: r.Symbol, Decimals: r.Decimals}
	}
	return out, nil
}

// BaseQty converts qty in unitID to the item's base unit (3 decimals),
// validating that the unit is set up for the item.
func BaseQty(units map[int64]map[int32]unitInfo, fieldPrefix string, itemID int64, unitID int64, qty decimal.Decimal) (decimal.Decimal, decimal.Decimal, string, error) {
	u, ok := units[itemID][int32(unitID)]
	if !ok {
		return decimal.Zero, decimal.Zero, "", httpx.Validation(fieldPrefix+".unit_id", "unit is not set up for this item")
	}
	if err := CheckDecimals(fieldPrefix+".qty", qty, u.Decimals); err != nil {
		return decimal.Zero, decimal.Zero, "", err
	}
	return qty.Mul(u.Factor).Round(3), u.Factor, u.Symbol, nil
}

// ---------------------------------------------------------------- transfers

func toTransfer(t dbq.StockTransfer, createdBy string, lines []dbq.TransferLinesRow) api.Transfer {
	out := api.Transfer{ID: t.ID, DocNo: t.DocNo, TransferDate: api.Date(t.TransferDate),
		FromLocationID: int64(t.FromLocationID), ToLocationID: int64(t.ToLocationID), CreatedByName: createdBy,
		Lines: []api.TransferLine{}}
	for _, l := range lines {
		if l.TransferID == t.ID {
			out.Lines = append(out.Lines, api.TransferLine{ItemID: l.ItemID, ItemName: l.ItemName, BaseQty: api.Q(l.BaseQty), Unit: l.UnitSymbol})
		}
	}
	return out
}

func (h handler) listTransfers(w http.ResponseWriter, r *http.Request) error {
	page, err := httpx.ParsePage(r)
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
	p := dbq.ListTransfersParams{FromDate: from, ToDate: to, Lim: page.Limit(), Off: page.Offset()}
	if loc != nil {
		l := int16(*loc)
		p.LocationID = &l
	}
	ctx := r.Context()
	rows, err := h.a.Q.ListTransfers(ctx, p)
	if err != nil {
		return err
	}
	ids := make([]int64, len(rows))
	var total int64
	for i, t := range rows {
		ids[i], total = t.ID, t.Total
	}
	lines, err := h.a.Q.TransferLines(ctx, ids)
	if err != nil {
		return err
	}
	out := make([]api.Transfer, len(rows))
	for i, t := range rows {
		out[i] = toTransfer(dbq.StockTransfer{ID: t.ID, DocNo: t.DocNo, Fy: t.Fy, TransferDate: t.TransferDate,
			FromLocationID: t.FromLocationID, ToLocationID: t.ToLocationID}, t.CreatedByName, lines)
	}
	httpx.JSON(w, http.StatusOK, httpx.NewList(out, page, total))
	return nil
}

func getTransfer(ctx context.Context, q *dbq.Queries, id int64) (api.Transfer, error) {
	t, err := q.GetTransfer(ctx, id)
	if err != nil {
		return api.Transfer{}, notFound(err, "transfer")
	}
	lines, err := q.TransferLines(ctx, []int64{id})
	if err != nil {
		return api.Transfer{}, err
	}
	return toTransfer(dbq.StockTransfer{ID: t.ID, DocNo: t.DocNo, Fy: t.Fy, TransferDate: t.TransferDate,
		FromLocationID: t.FromLocationID, ToLocationID: t.ToLocationID}, t.CreatedByName, lines), nil
}

func (h handler) getTransfer(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	t, err := getTransfer(r.Context(), h.a.Q, id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, t)
	return nil
}

// createTransfer moves stock between locations in one transaction (FR-PU-04).
func (h handler) createTransfer(w http.ResponseWriter, r *http.Request) error {
	body, err := httpx.ReadBody(r)
	if err != nil {
		return err
	}
	var in api.TransferInput
	if err := httpx.DecodeBytes(body, &in); err != nil {
		return err
	}
	err = idem.Run(w, r, h.a, body, func(ctx context.Context, _ pgx.Tx, q *dbq.Queries) (idem.Result, error) {
		locs, err := activeLocations(ctx, q)
		if err != nil {
			return idem.Result{}, err
		}
		from, to := int16(in.FromLocationID), int16(in.ToLocationID)
		if !locs[from] {
			return idem.Result{}, httpx.Validation("from_location_id", "unknown or inactive location")
		}
		if !locs[to] {
			return idem.Result{}, httpx.Validation("to_location_id", "unknown or inactive location")
		}
		itemIDs := make([]int64, len(in.Lines))
		for i, l := range in.Lines {
			itemIDs[i] = l.ItemID
		}
		units, err := UnitFactors(ctx, q, itemIDs)
		if err != nil {
			return idem.Result{}, err
		}
		base := make([]decimal.Decimal, len(in.Lines))
		needs := make([]Need, len(in.Lines))
		for i, l := range in.Lines {
			bq, _, _, err := BaseQty(units, field("lines", i, ""), l.ItemID, l.UnitID, num.Parse(l.Qty))
			if err != nil {
				return idem.Result{}, err
			}
			base[i] = bq
			needs[i] = Need{LineNo: i + 1, ItemID: l.ItemID, LocationID: from, Qty: bq}
		}
		short, err := CheckShortages(ctx, q, needs, true)
		if err != nil {
			return idem.Result{}, err
		}
		if len(short) > 0 {
			return idem.Result{}, &ShortageError{Lines: short}
		}
		date := h.a.DateOr(httpx.ParseDate(in.TransferDate))
		docNo, err := q.NextDocNo(ctx, dbq.NextDocNoParams{DocType: dbq.DocKindTransfer, DocDate: date})
		if err != nil {
			return idem.Result{}, err
		}
		fy, err := q.FY(ctx, date)
		if err != nil {
			return idem.Result{}, err
		}
		user := auth.UserID(ctx)
		id, err := q.InsertTransfer(ctx, dbq.InsertTransferParams{DocNo: docNo, Fy: fy, TransferDate: date,
			FromLocationID: from, ToLocationID: to, VehicleNo: in.VehicleNo, Notes: in.Notes, CreatedBy: *user})
		if err != nil {
			return idem.Result{}, err
		}
		ref := dbq.DocKindTransfer
		for i, l := range in.Lines {
			lineID, err := q.InsertTransferLine(ctx, dbq.InsertTransferLineParams{TransferID: id, ItemID: l.ItemID, BaseQty: base[i]})
			if err != nil {
				return idem.Result{}, err
			}
			cost, err := q.StockOut(ctx, dbq.StockOutParams{ItemID: l.ItemID, LocationID: from, Qty: base[i],
				Kind: dbq.MovementKindTransferOut, RefType: ref, RefID: id, RefLine: &lineID, UserID: *user})
			if err != nil {
				return idem.Result{}, err
			}
			if err := q.StockIn(ctx, dbq.StockInParams{ItemID: l.ItemID, LocationID: to, Qty: base[i], Cost: &cost,
				Kind: dbq.MovementKindTransferIn, RefType: &ref, RefID: &id, RefLine: &lineID, UserID: user}); err != nil {
				return idem.Result{}, err
			}
		}
		t, err := getTransfer(ctx, q, id)
		if err != nil {
			return idem.Result{}, err
		}
		if err := audit.Log(ctx, q, "create", "stock_transfer", audit.ID(id), nil, t); err != nil {
			return idem.Result{}, err
		}
		return idem.Result{Status: http.StatusCreated, Body: t, Location: "/api/v1/stock-transfers/" + itoa64(id)}, nil
	})
	return shortageOr(err)
}

// shortageOr converts a ShortageError into the 422 problem.
func shortageOr(err error) error {
	if se, ok := err.(*ShortageError); ok {
		return ShortageProblem(se.Lines, false)
	}
	return err
}

func itoa64(n int64) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa64(n/10) + string(rune('0'+n%10))
}

// ---------------------------------------------------------------- adjustments

func toAdjustment(a dbq.StockAdjustment, createdBy string, lines []dbq.AdjustmentLinesRow) api.Adjustment {
	out := api.Adjustment{ID: a.ID, DocNo: a.DocNo, AdjDate: api.Date(a.AdjDate), LocationID: int64(a.LocationID),
		Reason: string(a.Reason), Notes: a.Notes, CreatedByName: createdBy, Lines: []api.AdjustmentLine{}}
	for _, l := range lines {
		if l.AdjustmentID == a.ID {
			out.Lines = append(out.Lines, api.AdjustmentLine{ItemID: l.ItemID, ItemName: l.ItemName, QtyChange: api.Q(l.QtyChange)})
		}
	}
	return out
}

func getAdjustment(ctx context.Context, q *dbq.Queries, id int64) (api.Adjustment, error) {
	a, err := q.GetAdjustment(ctx, id)
	if err != nil {
		return api.Adjustment{}, notFound(err, "adjustment")
	}
	lines, err := q.AdjustmentLines(ctx, []int64{id})
	if err != nil {
		return api.Adjustment{}, err
	}
	return toAdjustment(dbq.StockAdjustment{ID: a.ID, DocNo: a.DocNo, Fy: a.Fy, AdjDate: a.AdjDate, LocationID: a.LocationID,
		Reason: a.Reason, Notes: a.Notes}, a.CreatedByName, lines), nil
}

func (h handler) listAdjustments(w http.ResponseWriter, r *http.Request) error {
	page, err := httpx.ParsePage(r)
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
	p := dbq.ListAdjustmentsParams{FromDate: from, ToDate: to, Lim: page.Limit(), Off: page.Offset()}
	if loc != nil {
		l := int16(*loc)
		p.LocationID = &l
	}
	ctx := r.Context()
	rows, err := h.a.Q.ListAdjustments(ctx, p)
	if err != nil {
		return err
	}
	ids := make([]int64, len(rows))
	var total int64
	for i, a := range rows {
		ids[i], total = a.ID, a.Total
	}
	lines, err := h.a.Q.AdjustmentLines(ctx, ids)
	if err != nil {
		return err
	}
	out := make([]api.Adjustment, len(rows))
	for i, a := range rows {
		out[i] = toAdjustment(dbq.StockAdjustment{ID: a.ID, DocNo: a.DocNo, Fy: a.Fy, AdjDate: a.AdjDate,
			LocationID: a.LocationID, Reason: a.Reason, Notes: a.Notes}, a.CreatedByName, lines)
	}
	httpx.JSON(w, http.StatusOK, httpx.NewList(out, page, total))
	return nil
}

type adjLine struct {
	ItemID int64
	Change decimal.Decimal
}

// postAdjustment writes an adjustment document and its stock movements.
func (h handler) postAdjustment(ctx context.Context, q *dbq.Queries, loc int16, date time.Time, reason dbq.AdjustReason,
	notes *string, lines []adjLine) (api.Adjustment, error) {
	if reason == dbq.AdjustReasonOther && (notes == nil || *notes == "") {
		return api.Adjustment{}, httpx.Validation("notes", "notes are required when the reason is other")
	}
	items, err := q.StockAtLocation(ctx, dbq.StockAtLocationParams{LocationID: loc, Ids: adjIDs(lines)})
	if err != nil {
		return api.Adjustment{}, err
	}
	known := map[int64]dbq.StockAtLocationRow{}
	for _, it := range items {
		known[it.ID] = it
	}
	var needs []Need
	for i, l := range lines {
		if _, ok := known[l.ItemID]; !ok {
			return api.Adjustment{}, httpx.Validation(field("lines", i, "item_id"), "unknown item")
		}
		if err := CheckDecimals(field("lines", i, "qty_change"), l.Change, 3); err != nil {
			return api.Adjustment{}, err
		}
		if l.Change.IsNegative() {
			needs = append(needs, Need{LineNo: i + 1, ItemID: l.ItemID, LocationID: loc, Qty: l.Change.Neg()})
		}
	}
	short, err := CheckShortages(ctx, q, needs, true)
	if err != nil {
		return api.Adjustment{}, err
	}
	if len(short) > 0 {
		return api.Adjustment{}, &ShortageError{Lines: short}
	}
	docNo, err := q.NextDocNo(ctx, dbq.NextDocNoParams{DocType: dbq.DocKindAdjustment, DocDate: date})
	if err != nil {
		return api.Adjustment{}, err
	}
	fy, err := q.FY(ctx, date)
	if err != nil {
		return api.Adjustment{}, err
	}
	user := auth.UserID(ctx)
	id, err := q.InsertAdjustment(ctx, dbq.InsertAdjustmentParams{DocNo: docNo, Fy: fy, AdjDate: date, LocationID: loc,
		Reason: reason, Notes: notes, CreatedBy: *user})
	if err != nil {
		return api.Adjustment{}, err
	}
	ref := dbq.DocKindAdjustment
	for _, l := range lines {
		lineID, err := q.InsertAdjustmentLine(ctx, dbq.InsertAdjustmentLineParams{AdjustmentID: id, ItemID: l.ItemID, QtyChange: l.Change})
		if err != nil {
			return api.Adjustment{}, err
		}
		if l.Change.IsPositive() {
			err = q.StockIn(ctx, dbq.StockInParams{ItemID: l.ItemID, LocationID: loc, Qty: l.Change,
				Kind: dbq.MovementKindAdjustment, RefType: &ref, RefID: &id, RefLine: &lineID, UserID: user})
		} else {
			_, err = q.StockOut(ctx, dbq.StockOutParams{ItemID: l.ItemID, LocationID: loc, Qty: l.Change.Neg(),
				Kind: dbq.MovementKindAdjustment, RefType: ref, RefID: id, RefLine: &lineID, UserID: *user})
		}
		if err != nil {
			return api.Adjustment{}, err
		}
	}
	out, err := getAdjustment(ctx, q, id)
	if err != nil {
		return out, err
	}
	return out, audit.Log(ctx, q, "create", "stock_adjustment", audit.ID(id), nil, out)
}

func adjIDs(lines []adjLine) []int64 {
	ids := make([]int64, len(lines))
	for i, l := range lines {
		ids[i] = l.ItemID
	}
	return ids
}

// createAdjustment: stock in/out with a mandatory reason (FR-PU-05).
// Quantities are in the item's base unit.
func (h handler) createAdjustment(w http.ResponseWriter, r *http.Request) error {
	body, err := httpx.ReadBody(r)
	if err != nil {
		return err
	}
	var in api.AdjustmentInput
	if err := httpx.DecodeBytes(body, &in); err != nil {
		return err
	}
	err = idem.Run(w, r, h.a, body, func(ctx context.Context, _ pgx.Tx, q *dbq.Queries) (idem.Result, error) {
		locs, err := activeLocations(ctx, q)
		if err != nil {
			return idem.Result{}, err
		}
		if !locs[int16(in.LocationID)] {
			return idem.Result{}, httpx.Validation("location_id", "unknown or inactive location")
		}
		lines := make([]adjLine, len(in.Lines))
		for i, l := range in.Lines {
			lines[i] = adjLine{ItemID: l.ItemID, Change: num.Parse(l.QtyChange)}
		}
		out, err := h.postAdjustment(ctx, q, int16(in.LocationID), h.a.DateOr(httpx.ParseDate(in.AdjDate)),
			dbq.AdjustReason(in.Reason), in.Notes, lines)
		if err != nil {
			return idem.Result{}, err
		}
		return idem.Result{Status: http.StatusCreated, Body: out}, nil
	})
	return shortageOr(err)
}

// ---------------------------------------------------------------- physical counts

func (h handler) countLines(ctx context.Context, q *dbq.Queries, in api.StockCountInput) ([]api.StockCountLine, error) {
	ids := make([]int64, len(in.Lines))
	for i, l := range in.Lines {
		ids[i] = l.ItemID
	}
	rows, err := q.StockAtLocation(ctx, dbq.StockAtLocationParams{LocationID: int16(in.LocationID), Ids: ids})
	if err != nil {
		return nil, err
	}
	byID := map[int64]dbq.StockAtLocationRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	out := make([]api.StockCountLine, len(in.Lines))
	seen := map[int64]bool{}
	for i, l := range in.Lines {
		it, ok := byID[l.ItemID]
		if !ok {
			return nil, httpx.Validation(field("lines", i, "item_id"), "unknown item")
		}
		if seen[l.ItemID] {
			return nil, httpx.Validation(field("lines", i, "item_id"), "item counted twice")
		}
		seen[l.ItemID] = true
		counted := num.Parse(l.CountedQty)
		diff := counted.Sub(it.Qty)
		out[i] = api.StockCountLine{ItemID: it.ID, ItemName: it.Name, SystemQty: api.Q(it.Qty), CountedQty: api.Q(counted),
			Difference: api.Q(diff), ValueDifference: api.M(diff.Mul(it.AvgCost).Round(2))}
	}
	return out, nil
}

// previewCount compares counted with system quantities (FR-PU-06).
func (h handler) previewCount(w http.ResponseWriter, r *http.Request) error {
	var in api.StockCountInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	locs, err := activeLocations(ctx, h.a.Q)
	if err != nil {
		return err
	}
	if !locs[int16(in.LocationID)] {
		return httpx.Validation("location_id", "unknown or inactive location")
	}
	lines, err := h.countLines(ctx, h.a.Q, in)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, api.StockCountPreview{LocationID: in.LocationID, Lines: lines})
	return nil
}

// postCount posts the differences as a count_correction adjustment.
func (h handler) postCount(w http.ResponseWriter, r *http.Request) error {
	body, err := httpx.ReadBody(r)
	if err != nil {
		return err
	}
	var in api.StockCountInput
	if err := httpx.DecodeBytes(body, &in); err != nil {
		return err
	}
	err = idem.Run(w, r, h.a, body, func(ctx context.Context, _ pgx.Tx, q *dbq.Queries) (idem.Result, error) {
		locs, err := activeLocations(ctx, q)
		if err != nil {
			return idem.Result{}, err
		}
		if !locs[int16(in.LocationID)] {
			return idem.Result{}, httpx.Validation("location_id", "unknown or inactive location")
		}
		// Lock the location's stock for these items before reading it.
		ids := make([]int64, len(in.Lines))
		for i, l := range in.Lines {
			ids[i] = l.ItemID
		}
		if _, err := q.LockStockRows(ctx, ids); err != nil {
			return idem.Result{}, err
		}
		counted, err := h.countLines(ctx, q, in)
		if err != nil {
			return idem.Result{}, err
		}
		var lines []adjLine
		for _, c := range counted {
			if !c.Difference.D().IsZero() {
				lines = append(lines, adjLine{ItemID: c.ItemID, Change: c.Difference.D()})
			}
		}
		if len(lines) == 0 {
			return idem.Result{}, httpx.Validation("lines", "counted quantities match the system; nothing to post")
		}
		notes := "Physical stock count"
		out, err := h.postAdjustment(ctx, q, int16(in.LocationID), h.a.Today(), dbq.AdjustReasonCountCorrection, &notes, lines)
		if err != nil {
			return idem.Result{}, err
		}
		return idem.Result{Status: http.StatusCreated, Body: out}, nil
	})
	return shortageOr(err)
}
