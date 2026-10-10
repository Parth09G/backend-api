package inventory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
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
	"billing-api/internal/num"
)

// ItemRoutes mounts /items.
func ItemRoutes(a *app.App) func(chi.Router) {
	h := handler{a}
	return func(r chi.Router) {
		r.Get("/", httpx.H(h.listItems))
		r.Post("/", httpx.H(h.createItem))
		r.Post("/import", httpx.H(h.importItems))
		r.Get("/{id}", httpx.H(h.getItem))
		r.Patch("/{id}", httpx.H(h.updateItem))
		r.With(auth.SuperAdmin).Post("/{id}/deactivate", httpx.H(h.deactivateItem))
		r.Get("/{id}/movements", httpx.H(h.movements))
		r.Get("/{id}/price-hints", httpx.H(h.priceHints))
	}
}

// LoadItems returns API items for ids, in the same order.
func LoadItems(ctx context.Context, q *dbq.Queries, ids []int64) ([]api.Item, error) {
	if len(ids) == 0 {
		return []api.Item{}, nil
	}
	rows, err := q.ItemsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	units, err := q.ItemUnitsByItems(ctx, ids)
	if err != nil {
		return nil, err
	}
	stock, err := q.ItemStockByItems(ctx, ids)
	if err != nil {
		return nil, err
	}
	byUnits := map[int64][]api.ItemUnit{}
	for _, u := range units {
		byUnits[u.ItemID] = append(byUnits[u.ItemID], api.ItemUnit{UnitID: int64(u.UnitID), Symbol: u.Symbol,
			FactorToBase: api.R(u.FactorToBase)})
	}
	byStock := map[int64][]api.StockByLocation{}
	for _, s := range stock {
		byStock[s.ItemID] = append(byStock[s.ItemID], api.StockByLocation{LocationID: int64(s.LocationID),
			LocationName: s.LocationName, Qty: api.Q(s.Qty)})
	}
	byID := map[int64]api.Item{}
	for _, it := range rows {
		attrs := map[string]any{}
		_ = json.Unmarshal(it.Attributes, &attrs)
		status := "ok"
		switch {
		case !it.StockTotal.IsPositive():
			status = "out"
		case it.StockTotal.LessThanOrEqual(it.ReorderLevel):
			status = "low"
		}
		x := api.Item{
			ID: it.ID, CategoryID: int64(it.CategoryID), CategoryName: it.CategoryName, Name: it.Name,
			Attributes: attrs, SKU: it.Sku, HSNCode: it.HsnCode, GSTRate: num.Trim(it.GstRate),
			BaseUnit: api.Unit{ID: int64(it.BaseUnitID), Name: it.UnitName, Symbol: it.UnitSymbol, UQCCode: it.UnitUqc,
				Decimals: int32(it.UnitDecimals)},
			Units: byUnits[it.ID], ReorderLevel: api.Q(it.ReorderLevel), AvgCost: api.R(it.AvgCost),
			LastPurchaseRate: api.RP(it.LastPurchaseRate), IsActive: it.IsActive, StockTotal: api.Q(it.StockTotal),
			Stock: byStock[it.ID], StockValue: api.M(it.StockTotal.Mul(it.AvgCost).Round(2)), StockStatus: status,
			UpdatedAt: it.UpdatedAt,
		}
		if x.Units == nil {
			x.Units = []api.ItemUnit{}
		}
		if x.Stock == nil {
			x.Stock = []api.StockByLocation{}
		}
		byID[it.ID] = x
	}
	out := make([]api.Item, 0, len(ids))
	for _, id := range ids {
		if it, ok := byID[id]; ok {
			out = append(out, it)
		}
	}
	return out, nil
}

// Terms splits a search string into lower-case words (all must match).
func Terms(q string) []string {
	out := []string{}
	for _, t := range strings.Fields(strings.ToLower(q)) {
		out = append(out, strings.NewReplacer("%", "", "_", "").Replace(t))
	}
	return out
}

// attrFilters reads attr[brand]=Tata&attr[size]=12 mm.
func attrFilters(r *http.Request) []byte {
	m := map[string]string{}
	for k, v := range r.URL.Query() {
		if strings.HasPrefix(k, "attr[") && strings.HasSuffix(k, "]") && len(v) > 0 {
			m[k[5:len(k)-1]] = v[0]
		}
	}
	b, _ := json.Marshal(m)
	return b
}

func (h handler) listItems(w http.ResponseWriter, r *http.Request) error {
	page, err := httpx.ParsePage(r)
	if err != nil {
		return err
	}
	catID, err := httpx.QInt64(r, "category_id")
	if err != nil {
		return err
	}
	locID, err := httpx.QInt64(r, "location_id")
	if err != nil {
		return err
	}
	status, err := httpx.QEnum(r, "stock_status", "ok", "low", "out")
	if err != nil {
		return err
	}
	inactive, err := httpx.QBool(r, "include_inactive")
	if err != nil {
		return err
	}
	sort, desc, err := httpx.Sort(r, "name", "name", "category", "stock_total", "stock_value", "updated_at")
	if err != nil {
		return err
	}
	p := dbq.ListItemIDsParams{StockStatus: status, Sort: sort, Descending: desc, Off: page.Offset(), Lim: page.Limit(),
		IncludeInactive: inactive, Terms: Terms(httpx.QString(r, "q")), Attrs: attrFilters(r)}
	if catID != nil {
		c := int32(*catID)
		p.CategoryID = &c
	}
	if locID != nil {
		l := int16(*locID)
		p.LocationID = &l
	}
	rows, err := h.a.Q.ListItemIDs(r.Context(), p)
	if err != nil {
		return err
	}
	ids := make([]int64, len(rows))
	var total int64
	for i, row := range rows {
		ids[i] = row.ID
		total = row.Total
	}
	items, err := LoadItems(r.Context(), h.a.Q, ids)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, httpx.NewList(items, page, total))
	return nil
}

func (h handler) getItem(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	items, err := LoadItems(r.Context(), h.a.Q, []int64{id})
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return httpx.NotFound("item")
	}
	httpx.JSON(w, http.StatusOK, items[0])
	return nil
}

// normalizeAttributes checks values against the category definition
// (FR-IN-02): unknown codes are rejected, required ones must be present,
// numbers are stored as JSON numbers and select values use the option's
// spelling. It also builds the default item name from part_of_name values.
func normalizeAttributes(defs []dbq.CategoryAttribute, in map[string]any) (map[string]any, string, []httpx.FieldError) {
	out := map[string]any{}
	var errs []httpx.FieldError
	known := map[string]dbq.CategoryAttribute{}
	for _, d := range defs {
		known[d.Code] = d
	}
	for k := range in {
		if _, ok := known[k]; !ok {
			errs = append(errs, httpx.FieldError{Field: "attributes." + k, Message: "is not an attribute of this category"})
		}
	}
	var nameParts []string
	for _, d := range defs {
		f := "attributes." + d.Code
		raw, present := in[d.Code]
		s := strings.TrimSpace(fmt.Sprint(raw))
		if !present || raw == nil || s == "" {
			if d.IsRequired {
				errs = append(errs, httpx.FieldError{Field: f, Message: "is required"})
			}
			continue
		}
		switch d.DataType {
		case dbq.AttrTypeNumber:
			dec, err := decimal.NewFromString(s)
			if err != nil {
				errs = append(errs, httpx.FieldError{Field: f, Message: "must be a number"})
				continue
			}
			out[d.Code] = json.Number(dec.String())
			s = dec.String()
		case dbq.AttrTypeSelect:
			i := slices.IndexFunc(d.Options, func(o string) bool { return strings.EqualFold(o, s) })
			if i < 0 {
				errs = append(errs, httpx.FieldError{Field: f, Message: "must be one of: " + strings.Join(d.Options, ", ")})
				continue
			}
			s = d.Options[i]
			out[d.Code] = s
		default:
			out[d.Code] = s
		}
		if d.PartOfName {
			if d.ValueUnit != nil && *d.ValueUnit != "" && d.DataType == dbq.AttrTypeNumber {
				s += " " + *d.ValueUnit
			}
			nameParts = append(nameParts, s)
		}
	}
	return out, strings.Join(nameParts, " "), errs
}

type itemPlan struct {
	params   dbq.InsertItemParams
	units    map[int32]decimal.Decimal // unit -> factor (base unit = 1)
	decimals map[int32]int16
}

// planItem validates an ItemInput against its category.
func (h handler) planItem(ctx context.Context, q *dbq.Queries, in api.ItemInput) (itemPlan, error) {
	cat, err := q.GetCategory(ctx, int32(in.CategoryID))
	if err != nil {
		return itemPlan{}, httpx.Validation("category_id", "unknown category")
	}
	if !cat.IsActive {
		return itemPlan{}, httpx.Validation("category_id", "category is inactive")
	}
	defs, err := q.CategoryAttributes(ctx, []int32{cat.ID})
	if err != nil {
		return itemPlan{}, err
	}
	attrs, autoName, errs := normalizeAttributes(defs, in.Attributes)
	name := autoName
	if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
		name = strings.TrimSpace(*in.Name)
	}
	if name == "" {
		errs = append(errs, httpx.FieldError{Field: "name", Message: "is required when no attribute is part of the name"})
	}

	catUnits, err := q.CategoryUnits(ctx, []int32{cat.ID})
	if err != nil {
		return itemPlan{}, err
	}
	allowed := map[int32]bool{}
	for _, u := range catUnits {
		allowed[u.ID] = true
	}
	unitOK := func(id int32) bool { return len(allowed) == 0 || allowed[id] }
	allUnits, err := q.ListUnits(ctx)
	if err != nil {
		return itemPlan{}, err
	}
	decimals := map[int32]int16{}
	for _, u := range allUnits {
		decimals[u.ID] = u.Decimals
	}
	base := int32(in.BaseUnitID)
	if _, ok := decimals[base]; !ok || !unitOK(base) {
		errs = append(errs, httpx.FieldError{Field: "base_unit_id", Message: "unit is unknown or not allowed for this category"})
	}
	units := map[int32]decimal.Decimal{base: decimal.NewFromInt(1)}
	for i, u := range in.Units {
		id := int32(u.UnitID)
		f := num.Parse(u.FactorToBase)
		switch {
		case !hasUnit(allUnits, id) || !unitOK(id):
			errs = append(errs, httpx.FieldError{Field: field("units", i, "unit_id"), Message: "unit is unknown or not allowed for this category"})
		case id == base && !f.Equal(decimal.NewFromInt(1)):
			errs = append(errs, httpx.FieldError{Field: field("units", i, "factor_to_base"), Message: "the base unit's factor is 1"})
		default:
			units[id] = f
		}
	}
	reorder := decimal.Zero
	if in.ReorderLevel != nil {
		reorder = num.Parse(*in.ReorderLevel)
	}
	if !httpx.GSTRates[in.GSTRate] {
		errs = append(errs, httpx.FieldError{Field: "gst_rate", Message: "invalid GST rate"})
	}
	if len(errs) > 0 {
		return itemPlan{}, httpx.ValidationErrors(errs)
	}
	attrJSON, _ := json.Marshal(attrs)
	sku := in.SKU
	if sku != nil && strings.TrimSpace(*sku) == "" {
		sku = nil
	}
	return itemPlan{
		params: dbq.InsertItemParams{CategoryID: cat.ID, Name: name, Attributes: attrJSON, Sku: sku,
			HsnCode: strings.TrimSpace(in.HSNCode), GstRate: num.Parse(in.GSTRate), BaseUnitID: base,
			ReorderLevel: reorder, CreatedBy: auth.UserID(ctx)},
		units: units, decimals: decimals,
	}, nil
}

func hasUnit(units []dbq.Unit, id int32) bool {
	return slices.ContainsFunc(units, func(u dbq.Unit) bool { return u.ID == id })
}

// CheckDecimals rejects quantities with more decimals than the unit allows.
func CheckDecimals(fieldName string, qty decimal.Decimal, decimals int16) error {
	if qty.Exponent() < 0 && !qty.Equal(qty.Round(int32(decimals))) {
		return httpx.Validation(fieldName, fmt.Sprintf("this unit allows %d decimal places", decimals))
	}
	return nil
}

func (h handler) createItem(w http.ResponseWriter, r *http.Request) error {
	var in api.ItemInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	var out api.Item
	err := h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		id, err := h.insertItem(ctx, q, in)
		if err != nil {
			return err
		}
		items, err := LoadItems(ctx, q, []int64{id})
		if err != nil {
			return err
		}
		out = items[0]
		return audit.Log(ctx, q, "create", "item", audit.ID(id), nil, out)
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusCreated, out)
	return nil
}

// insertItem validates and creates an item with its units and opening
// stock (shared by POST /items and the Excel import).
func (h handler) insertItem(ctx context.Context, q *dbq.Queries, in api.ItemInput) (int64, error) {
	plan, err := h.planItem(ctx, q, in)
	if err != nil {
		return 0, err
	}
	if dup, err := q.FindDuplicateItem(ctx, dbq.FindDuplicateItemParams{CategoryID: plan.params.CategoryID,
		Attributes: plan.params.Attributes, ExcludeID: 0}); err == nil {
		return 0, duplicateItem(dup)
	} else if err != pgx.ErrNoRows {
		return 0, err
	}
	id, err := q.InsertItem(ctx, plan.params)
	if err != nil {
		return 0, err
	}
	for unitID, f := range plan.units {
		if err := q.UpsertItemUnit(ctx, dbq.UpsertItemUnitParams{ItemID: id, UnitID: unitID, FactorToBase: f}); err != nil {
			return 0, err
		}
	}
	locs, err := activeLocations(ctx, q)
	if err != nil {
		return 0, err
	}
	for i, os := range in.OpeningStock {
		if !locs[int16(os.LocationID)] {
			return 0, httpx.Validation(field("opening_stock", i, "location_id"), "unknown or inactive location")
		}
		qty := num.Parse(os.Qty)
		if err := CheckDecimals(field("opening_stock", i, "qty"), qty, plan.decimals[plan.params.BaseUnitID]); err != nil {
			return 0, err
		}
		err := q.StockIn(ctx, dbq.StockInParams{ItemID: id, LocationID: int16(os.LocationID), Qty: qty,
			Cost: num.ParsePtr(os.UnitCost), Kind: dbq.MovementKindOpening, UserID: auth.UserID(ctx)})
		if err != nil {
			return 0, err
		}
	}
	return id, nil
}

func duplicateItem(id int64) error {
	return &httpx.Problem{Status: http.StatusConflict, Code: httpx.CodeDuplicate,
		Detail: fmt.Sprintf("an item with the same category and attributes already exists (id %d)", id),
		Errors: []httpx.FieldError{{Field: "attributes", Message: "duplicate of item " + fmt.Sprint(id)}}}
}

func activeLocations(ctx context.Context, q *dbq.Queries) (map[int16]bool, error) {
	rows, err := q.ActiveLocations(ctx)
	if err != nil {
		return nil, err
	}
	m := map[int16]bool{}
	for _, l := range rows {
		m[l.ID] = true
	}
	return m, nil
}

// updateItem replaces the item's fields. Once the item has stock movements
// its base unit and existing unit factors are frozen (FR-IN-07); new units
// can still be added.
func (h handler) updateItem(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in api.ItemInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	if len(in.OpeningStock) > 0 {
		return httpx.Validation("opening_stock", "only accepted when the item is created; use a stock adjustment")
	}
	ctx := r.Context()
	var out api.Item
	err = h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		cur, err := q.LockItem(ctx, id)
		if err != nil {
			return notFound(err, "item")
		}
		before, err := LoadItems(ctx, q, []int64{id})
		if err != nil {
			return err
		}
		plan, err := h.planItem(ctx, q, in)
		if err != nil {
			return err
		}
		if dup, err := q.FindDuplicateItem(ctx, dbq.FindDuplicateItemParams{CategoryID: plan.params.CategoryID,
			Attributes: plan.params.Attributes, ExcludeID: id}); err == nil {
			return duplicateItem(dup)
		} else if err != pgx.ErrNoRows {
			return err
		}
		moved, err := q.ItemHasMovements(ctx, id)
		if err != nil {
			return err
		}
		if moved {
			if plan.params.BaseUnitID != cur.BaseUnitID {
				return httpx.Conflict(httpx.CodeConflict, "the base unit cannot change once the item has transactions")
			}
			for _, u := range before[0].Units {
				f, keep := plan.units[int32(u.UnitID)]
				if !keep {
					return httpx.Conflict(httpx.CodeConflict, "unit "+u.Symbol+" cannot be removed once the item has transactions")
				}
				if !f.Equal(u.FactorToBase.D()) {
					return httpx.Conflict(httpx.CodeConflict, "unit factors cannot change once the item has transactions; add a new unit instead")
				}
			}
		}
		if _, err := q.UpdateItem(ctx, dbq.UpdateItemParams{ID: id, CategoryID: plan.params.CategoryID,
			Name: plan.params.Name, Attributes: plan.params.Attributes, Sku: plan.params.Sku, HsnCode: plan.params.HsnCode,
			GstRate: plan.params.GstRate, BaseUnitID: plan.params.BaseUnitID, ReorderLevel: plan.params.ReorderLevel}); err != nil {
			return err
		}
		keep := make([]int32, 0, len(plan.units))
		for unitID, f := range plan.units {
			keep = append(keep, unitID)
			if err := q.UpsertItemUnit(ctx, dbq.UpsertItemUnitParams{ItemID: id, UnitID: unitID, FactorToBase: f}); err != nil {
				return err
			}
		}
		if err := q.DeleteItemUnitsExcept(ctx, dbq.DeleteItemUnitsExceptParams{ItemID: id, Keep: keep}); err != nil {
			return err
		}
		items, err := LoadItems(ctx, q, []int64{id})
		if err != nil {
			return err
		}
		out = items[0]
		return audit.Log(ctx, q, "update", "item", audit.ID(id), before[0], out)
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, out)
	return nil
}

func (h handler) deactivateItem(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	err = h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		n, err := q.SetItemActive(ctx, dbq.SetItemActiveParams{ID: id, IsActive: false})
		if err != nil {
			return err
		}
		if n == 0 {
			return httpx.NotFound("item")
		}
		return audit.Log(ctx, q, "deactivate", "item", audit.ID(id), nil, nil)
	})
	if err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}

func (h handler) movements(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	page, err := httpx.ParsePage(r)
	if err != nil {
		return err
	}
	locID, err := httpx.QInt64(r, "location_id")
	if err != nil {
		return err
	}
	from, to, err := httpx.Range(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	if _, err := h.a.Q.GetItemByID(ctx, id); err != nil {
		return notFound(err, "item")
	}
	p := dbq.ItemMovementsParams{ItemID: id, FromDate: from, ToDate: to, Lim: page.Limit(), Off: page.Offset()}
	if locID != nil {
		l := int16(*locID)
		p.LocationID = &l
	}
	rows, err := h.a.Q.ItemMovements(ctx, p)
	if err != nil {
		return err
	}
	out := make([]api.StockMovement, len(rows))
	var total int64
	for i, m := range rows {
		total = m.Total
		mv := api.StockMovement{ID: m.ID, Date: m.CreatedAt, LocationID: int64(m.LocationID), LocationName: m.LocationName,
			Kind: string(m.Kind), QtyIn: api.Q(m.QtyIn), QtyOut: api.Q(m.QtyOut), Balance: api.Q(m.Balance),
			UnitCost: api.R(m.UnitCost), RefID: m.RefID, RefNo: m.RefNo, UserName: m.UserName}
		if m.RefType != nil {
			mv.RefType = string(*m.RefType)
		}
		out[i] = mv
	}
	httpx.JSON(w, http.StatusOK, httpx.NewList(out, page, total))
	return nil
}

// priceHints: there is no master selling price (BR-05), so billing shows
// recent rates and costs. With unit_id, costs are converted to that unit.
func (h handler) priceHints(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	partyID, err := httpx.QInt64(r, "party_id")
	if err != nil {
		return err
	}
	unitID, err := httpx.QInt64(r, "unit_id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	items, err := LoadItems(ctx, h.a.Q, []int64{id})
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return httpx.NotFound("item")
	}
	it := items[0]
	factor := decimal.NewFromInt(1)
	unit := it.BaseUnit.Symbol
	var unitFilter *int32
	if unitID != nil {
		idx := slices.IndexFunc(it.Units, func(u api.ItemUnit) bool { return u.UnitID == *unitID })
		if idx < 0 {
			return httpx.Validation("unit_id", "unit is not set up for this item")
		}
		factor = it.Units[idx].FactorToBase.D()
		unit = it.Units[idx].Symbol
		u := int32(*unitID)
		unitFilter = &u
	}
	out := api.PriceHints{RecentRates: []api.RecentRate{}, AvgCost: api.R(it.AvgCost.D().Mul(factor)), Unit: unit}
	if it.LastPurchaseRate != nil {
		r := api.R(it.LastPurchaseRate.D().Mul(factor))
		out.LastPurchaseRate = &r
	}
	if partyID != nil {
		last, err := h.a.Q.LastRateToCustomer(ctx, dbq.LastRateToCustomerParams{ItemID: id, PartyID: partyID, UnitID: unitFilter})
		if err == nil {
			out.LastRateToCustomer = &api.LastRate{Rate: api.M(last.Rate), Unit: last.Unit, BillNo: last.BillNo, Date: api.Date(last.BillDate)}
		} else if err != pgx.ErrNoRows {
			return err
		}
	}
	recent, err := h.a.Q.RecentRates(ctx, dbq.RecentRatesParams{ItemID: id, UnitID: unitFilter})
	if err != nil {
		return err
	}
	for _, rr := range recent {
		out.RecentRates = append(out.RecentRates, api.RecentRate{Rate: api.M(rr.Rate), Unit: rr.Unit, Customer: rr.CustName,
			Date: api.Date(rr.BillDate)})
	}
	httpx.JSON(w, http.StatusOK, out)
	return nil
}
