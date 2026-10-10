// Package inventory handles locations, units, categories, items, stock
// transfers, adjustments, physical counts and the item stock ledger
// (FR-IN, FR-PU-04..07).
package inventory

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"billing-api/internal/api"
	"billing-api/internal/app"
	"billing-api/internal/audit"
	"billing-api/internal/auth"
	"billing-api/internal/db/dbq"
	"billing-api/internal/httpx"
	"billing-api/internal/num"
)

type handler struct{ a *app.App }

// LocationRoutes mounts /locations.
func LocationRoutes(a *app.App) func(chi.Router) {
	h := handler{a}
	return func(r chi.Router) {
		r.Get("/", httpx.H(h.listLocations))
		r.With(auth.SuperAdmin).Post("/", httpx.H(h.createLocation))
		r.With(auth.SuperAdmin).Patch("/{id}", httpx.H(h.updateLocation))
	}
}

// UnitRoutes mounts /units.
func UnitRoutes(a *app.App) func(chi.Router) {
	h := handler{a}
	return func(r chi.Router) {
		r.Get("/", httpx.H(h.listUnits))
		r.Post("/", httpx.H(h.createUnit))
		r.Patch("/{id}", httpx.H(h.updateUnit))
	}
}

// CategoryRoutes mounts /categories.
func CategoryRoutes(a *app.App) func(chi.Router) {
	h := handler{a}
	return func(r chi.Router) {
		r.Get("/", httpx.H(h.listCategories))
		r.Post("/", httpx.H(h.createCategory))
		r.Get("/{id}", httpx.H(h.getCategory))
		r.Patch("/{id}", httpx.H(h.updateCategory))
		r.With(auth.SuperAdmin).Post("/{id}/deactivate", httpx.H(h.deactivateCategory))
		r.Post("/{id}/attributes", httpx.H(h.addAttribute))
		r.Patch("/{id}/attributes/{attrId}", httpx.H(h.updateAttribute))
		r.With(auth.SuperAdmin).Delete("/{id}/attributes/{attrId}", httpx.H(h.deleteAttribute))
		r.Get("/{id}/import-template", httpx.H(h.importTemplate))
	}
}

// ---------------------------------------------------------------- locations

func toLocation(l dbq.Location) api.Location {
	return api.Location{ID: int64(l.ID), Name: l.Name, Kind: string(l.Kind), Address: l.Address,
		IsDefaultBilling: l.IsDefaultBilling, IsActive: l.IsActive}
}

func (h handler) listLocations(w http.ResponseWriter, r *http.Request) error {
	rows, err := h.a.Q.ListLocations(r.Context())
	if err != nil {
		return err
	}
	out := make([]api.Location, len(rows))
	for i, l := range rows {
		out[i] = toLocation(l)
	}
	httpx.JSON(w, http.StatusOK, out)
	return nil
}

func (h handler) createLocation(w http.ResponseWriter, r *http.Request) error {
	var in api.LocationInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	var row dbq.Location
	err := h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		def := in.IsDefaultBilling != nil && *in.IsDefaultBilling
		var err error
		row, err = q.InsertLocation(ctx, dbq.InsertLocationParams{Name: strings.TrimSpace(in.Name),
			Kind: dbq.LocationKind(in.Kind), Address: in.Address, IsDefaultBilling: false})
		if err != nil {
			return err
		}
		if def {
			if row, err = h.makeDefault(ctx, q, row); err != nil {
				return err
			}
		}
		return audit.Log(ctx, q, "create", "location", audit.ID(int64(row.ID)), nil, toLocation(row))
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusCreated, toLocation(row))
	return nil
}

func (h handler) makeDefault(ctx context.Context, q *dbq.Queries, l dbq.Location) (dbq.Location, error) {
	if err := q.ClearDefaultBilling(ctx, l.ID); err != nil {
		return l, err
	}
	return q.UpdateLocation(ctx, dbq.UpdateLocationParams{ID: l.ID, Name: l.Name, Kind: l.Kind, Address: l.Address,
		IsDefaultBilling: true, IsActive: l.IsActive})
}

func (h handler) updateLocation(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in api.LocationInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	var row dbq.Location
	err = h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		before, err := q.GetLocation(ctx, int16(id))
		if err != nil {
			return notFound(err, "location")
		}
		p := dbq.UpdateLocationParams{ID: before.ID, Name: strings.TrimSpace(in.Name), Kind: dbq.LocationKind(in.Kind),
			Address: in.Address, IsDefaultBilling: before.IsDefaultBilling, IsActive: before.IsActive}
		if in.IsActive != nil {
			p.IsActive = *in.IsActive
		}
		if in.IsDefaultBilling != nil && !*in.IsDefaultBilling && before.IsDefaultBilling {
			return httpx.Validation("is_default_billing", "choose another location as default instead of unsetting this one")
		}
		if !p.IsActive && before.IsDefaultBilling {
			return httpx.Validation("is_active", "the default billing location cannot be deactivated")
		}
		if row, err = q.UpdateLocation(ctx, p); err != nil {
			return err
		}
		if in.IsDefaultBilling != nil && *in.IsDefaultBilling && !before.IsDefaultBilling {
			if row, err = h.makeDefault(ctx, q, row); err != nil {
				return err
			}
		}
		return audit.Log(ctx, q, "update", "location", audit.ID(id), toLocation(before), toLocation(row))
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, toLocation(row))
	return nil
}

// ---------------------------------------------------------------- units

func toUnit(u dbq.Unit) api.Unit {
	return api.Unit{ID: int64(u.ID), Name: u.Name, Symbol: u.Symbol, UQCCode: u.UqcCode, Decimals: int32(u.Decimals)}
}

func (h handler) listUnits(w http.ResponseWriter, r *http.Request) error {
	rows, err := h.a.Q.ListUnits(r.Context())
	if err != nil {
		return err
	}
	out := make([]api.Unit, len(rows))
	for i, u := range rows {
		out[i] = toUnit(u)
	}
	httpx.JSON(w, http.StatusOK, out)
	return nil
}

func unitParams(in api.UnitInput) (name, symbol string, uqc *string, decimals int16) {
	decimals = 3
	if in.Decimals != nil {
		decimals = int16(*in.Decimals)
	}
	if in.UQCCode != nil {
		u := strings.ToUpper(strings.TrimSpace(*in.UQCCode))
		if u != "" {
			uqc = &u
		}
	}
	return strings.TrimSpace(in.Name), strings.TrimSpace(in.Symbol), uqc, decimals
}

func insertUnit(ctx context.Context, q *dbq.Queries, in api.UnitInput) (dbq.Unit, error) {
	name, symbol, uqc, dec := unitParams(in)
	return q.InsertUnit(ctx, dbq.InsertUnitParams{Name: name, Symbol: symbol, UqcCode: uqc, Decimals: dec})
}

func (h handler) createUnit(w http.ResponseWriter, r *http.Request) error {
	var in api.UnitInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	var row dbq.Unit
	err := h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		var err error
		if row, err = insertUnit(ctx, q, in); err != nil {
			return err
		}
		return audit.Log(ctx, q, "create", "unit", audit.ID(int64(row.ID)), nil, toUnit(row))
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusCreated, toUnit(row))
	return nil
}

func (h handler) updateUnit(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in api.UnitInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	var row dbq.Unit
	err = h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		before, err := q.GetUnit(ctx, int32(id))
		if err != nil {
			return notFound(err, "unit")
		}
		name, symbol, uqc, dec := unitParams(in)
		if in.Decimals == nil {
			dec = before.Decimals
		}
		row, err = q.UpdateUnit(ctx, dbq.UpdateUnitParams{ID: before.ID, Name: name, Symbol: symbol, UqcCode: uqc, Decimals: dec})
		if err != nil {
			return err
		}
		return audit.Log(ctx, q, "update", "unit", audit.ID(id), toUnit(before), toUnit(row))
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, toUnit(row))
	return nil
}

// ---------------------------------------------------------------- categories

func toAttribute(a dbq.CategoryAttribute) api.CategoryAttribute {
	opts := a.Options
	if opts == nil {
		opts = []string{}
	}
	return api.CategoryAttribute{ID: int64(a.ID), Code: a.Code, Label: a.Label, DataType: string(a.DataType),
		Options: opts, ValueUnit: a.ValueUnit, IsRequired: a.IsRequired, PartOfName: a.PartOfName,
		SortOrder: int32(a.SortOrder)}
}

// loadCategories builds API categories with their units and attributes.
func loadCategories(ctx context.Context, q *dbq.Queries, rows []dbq.ListCategoriesRow) ([]api.Category, error) {
	ids := make([]int32, len(rows))
	for i, c := range rows {
		ids[i] = c.ID
	}
	units, err := q.CategoryUnits(ctx, ids)
	if err != nil {
		return nil, err
	}
	attrs, err := q.CategoryAttributes(ctx, ids)
	if err != nil {
		return nil, err
	}
	byUnits := map[int32][]api.CategoryUnit{}
	for _, u := range units {
		byUnits[u.CategoryID] = append(byUnits[u.CategoryID], api.CategoryUnit{IsDefault: u.IsDefault,
			Unit: api.Unit{ID: int64(u.ID), Name: u.Name, Symbol: u.Symbol, UQCCode: u.UqcCode, Decimals: int32(u.Decimals)}})
	}
	byAttrs := map[int32][]api.CategoryAttribute{}
	for _, a := range attrs {
		byAttrs[a.CategoryID] = append(byAttrs[a.CategoryID], toAttribute(a))
	}
	out := make([]api.Category, len(rows))
	for i, c := range rows {
		var parent *int64
		if c.ParentID != nil {
			p := int64(*c.ParentID)
			parent = &p
		}
		out[i] = api.Category{ID: int64(c.ID), Name: c.Name, ParentID: parent, DefaultHSN: c.DefaultHsn,
			DefaultGST: num.TrimPtr(c.DefaultGst), Description: c.Description, IsActive: c.IsActive,
			TallyGroup: c.TallyGroup, Units: byUnits[c.ID], Attributes: byAttrs[c.ID], ItemCount: c.ItemCount}
		if out[i].Units == nil {
			out[i].Units = []api.CategoryUnit{}
		}
		if out[i].Attributes == nil {
			out[i].Attributes = []api.CategoryAttribute{}
		}
	}
	return out, nil
}

func getCategory(ctx context.Context, q *dbq.Queries, id int32) (api.Category, error) {
	c, err := q.GetCategory(ctx, id)
	if err != nil {
		return api.Category{}, notFound(err, "category")
	}
	list, err := loadCategories(ctx, q, []dbq.ListCategoriesRow{dbq.ListCategoriesRow(c)})
	if err != nil {
		return api.Category{}, err
	}
	return list[0], nil
}

func (h handler) listCategories(w http.ResponseWriter, r *http.Request) error {
	inactive, err := httpx.QBool(r, "include_inactive")
	if err != nil {
		return err
	}
	rows, err := h.a.Q.ListCategories(r.Context(), inactive)
	if err != nil {
		return err
	}
	out, err := loadCategories(r.Context(), h.a.Q, rows)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, out)
	return nil
}

func (h handler) getCategory(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	c, err := getCategory(r.Context(), h.a.Q, int32(id))
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, c)
	return nil
}

// setCategoryUnits replaces the category's units; new units are created on
// the spot (FR-IN-01). Exactly one default when any units are given.
func setCategoryUnits(ctx context.Context, q *dbq.Queries, catID int32, in []api.CategoryUnitInput) error {
	if err := q.DeleteCategoryUnits(ctx, catID); err != nil {
		return err
	}
	seen := map[int32]bool{}
	hasDefault := false
	for i, u := range in {
		if (u.UnitID == nil) == (u.NewUnit == nil) {
			return httpx.Validation(field("units", i, "unit_id"), "give either unit_id or new_unit")
		}
		var unitID int32
		if u.NewUnit != nil {
			if err := httpx.Validate(u.NewUnit); err != nil {
				return err
			}
			nu, err := insertUnit(ctx, q, *u.NewUnit)
			if err != nil {
				return err
			}
			unitID = nu.ID
		} else {
			if _, err := q.GetUnit(ctx, int32(*u.UnitID)); err != nil {
				return httpx.Validation(field("units", i, "unit_id"), "unknown unit")
			}
			unitID = int32(*u.UnitID)
		}
		if seen[unitID] {
			return httpx.Validation(field("units", i, "unit_id"), "unit listed twice")
		}
		seen[unitID] = true
		isDefault := u.IsDefault && !hasDefault
		hasDefault = hasDefault || isDefault
		if err := q.InsertCategoryUnit(ctx, dbq.InsertCategoryUnitParams{CategoryID: catID, UnitID: unitID, IsDefault: isDefault}); err != nil {
			return err
		}
	}
	return nil
}

func attrParams(in api.CategoryAttributeInput) (dbq.InsertCategoryAttributeParams, error) {
	p := dbq.InsertCategoryAttributeParams{Code: in.Code, Label: strings.TrimSpace(in.Label),
		DataType: dbq.AttrType(in.DataType), ValueUnit: in.ValueUnit, PartOfName: true}
	if in.DataType == "select" {
		if len(in.Options) == 0 {
			return p, httpx.Validation("options", "a select attribute needs options")
		}
		p.Options = in.Options
	}
	if in.IsRequired != nil {
		p.IsRequired = *in.IsRequired
	}
	if in.PartOfName != nil {
		p.PartOfName = *in.PartOfName
	}
	if in.SortOrder != nil {
		p.SortOrder = int16(*in.SortOrder)
	}
	return p, nil
}

func categoryParams(in api.CategoryInput) (dbq.InsertCategoryParams, error) {
	p := dbq.InsertCategoryParams{Name: strings.TrimSpace(in.Name), DefaultHsn: in.DefaultHSN,
		DefaultGst: num.ParsePtr(in.DefaultGST), Description: in.Description, TallyGroup: in.TallyGroup}
	if in.ParentID != nil {
		pid := int32(*in.ParentID)
		p.ParentID = &pid
	}
	return p, nil
}

func (h handler) createCategory(w http.ResponseWriter, r *http.Request) error {
	var in api.CategoryInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	var out api.Category
	err := h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		p, err := categoryParams(in)
		if err != nil {
			return err
		}
		id, err := q.InsertCategory(ctx, p)
		if err != nil {
			return err
		}
		if err := setCategoryUnits(ctx, q, id, in.Units); err != nil {
			return err
		}
		codes := map[string]bool{}
		for i, ai := range in.Attributes {
			if codes[ai.Code] {
				return httpx.Validation(field("attributes", i, "code"), "duplicate code")
			}
			codes[ai.Code] = true
			ap, err := attrParams(ai)
			if err != nil {
				return prefix(err, field("attributes", i, ""))
			}
			ap.CategoryID = id
			if _, err := q.InsertCategoryAttribute(ctx, ap); err != nil {
				return err
			}
		}
		if out, err = getCategory(ctx, q, id); err != nil {
			return err
		}
		return audit.Log(ctx, q, "create", "category", audit.ID(int64(id)), nil, out)
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusCreated, out)
	return nil
}

// updateCategory changes name, defaults and (when given) the unit list.
// Attributes are managed through /attributes.
func (h handler) updateCategory(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in api.CategoryInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	var out api.Category
	err = h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		before, err := getCategory(ctx, q, int32(id))
		if err != nil {
			return err
		}
		if in.ParentID != nil && *in.ParentID == id {
			return httpx.Validation("parent_id", "a category cannot be its own parent")
		}
		p, err := categoryParams(in)
		if err != nil {
			return err
		}
		if _, err := q.UpdateCategory(ctx, dbq.UpdateCategoryParams{ID: int32(id), Name: p.Name, ParentID: p.ParentID,
			DefaultHsn: p.DefaultHsn, DefaultGst: p.DefaultGst, Description: p.Description, TallyGroup: p.TallyGroup}); err != nil {
			return err
		}
		if in.Units != nil {
			if err := setCategoryUnits(ctx, q, int32(id), in.Units); err != nil {
				return err
			}
		}
		if len(in.Attributes) > 0 {
			return httpx.Validation("attributes", "manage attributes with /categories/{id}/attributes")
		}
		if out, err = getCategory(ctx, q, int32(id)); err != nil {
			return err
		}
		return audit.Log(ctx, q, "update", "category", audit.ID(id), before, out)
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, out)
	return nil
}

func (h handler) deactivateCategory(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	err = h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		n, err := q.CountActiveItemsInCategory(ctx, int32(id))
		if err != nil {
			return err
		}
		if n > 0 {
			return httpx.InvalidState("deactivate the category's items first")
		}
		rows, err := q.SetCategoryActive(ctx, dbq.SetCategoryActiveParams{ID: int32(id), IsActive: false})
		if err != nil {
			return err
		}
		if rows == 0 {
			return httpx.NotFound("category")
		}
		return audit.Log(ctx, q, "deactivate", "category", audit.ID(id), nil, nil)
	})
	if err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}

func (h handler) addAttribute(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in api.CategoryAttributeInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	var row dbq.CategoryAttribute
	err = h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		if _, err := q.GetCategory(ctx, int32(id)); err != nil {
			return notFound(err, "category")
		}
		p, err := attrParams(in)
		if err != nil {
			return err
		}
		p.CategoryID = int32(id)
		if row, err = q.InsertCategoryAttribute(ctx, p); err != nil {
			return err
		}
		return audit.Log(ctx, q, "create", "category_attribute", audit.ID(int64(row.ID)), nil, toAttribute(row))
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusCreated, toAttribute(row))
	return nil
}

func (h handler) updateAttribute(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	attrID, err := httpx.PathID(r, "attrId")
	if err != nil {
		return err
	}
	var in api.CategoryAttributeInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	var row dbq.CategoryAttribute
	err = h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		before, err := q.GetCategoryAttribute(ctx, dbq.GetCategoryAttributeParams{ID: int32(attrID), CategoryID: int32(id)})
		if err != nil {
			return notFound(err, "attribute")
		}
		if in.Code != before.Code {
			n, err := q.CountItemsWithAttribute(ctx, dbq.CountItemsWithAttributeParams{CategoryID: int32(id), Code: before.Code})
			if err != nil {
				return err
			}
			if n > 0 {
				return httpx.Validation("code", "the code cannot change while items use this attribute")
			}
		}
		p, err := attrParams(in)
		if err != nil {
			return err
		}
		row, err = q.UpdateCategoryAttribute(ctx, dbq.UpdateCategoryAttributeParams{ID: int32(attrID), CategoryID: int32(id),
			Code: p.Code, Label: p.Label, DataType: p.DataType, Options: p.Options, ValueUnit: p.ValueUnit,
			IsRequired: p.IsRequired, PartOfName: p.PartOfName, SortOrder: p.SortOrder})
		if err != nil {
			return err
		}
		return audit.Log(ctx, q, "update", "category_attribute", audit.ID(attrID), toAttribute(before), toAttribute(row))
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, toAttribute(row))
	return nil
}

func (h handler) deleteAttribute(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	attrID, err := httpx.PathID(r, "attrId")
	if err != nil {
		return err
	}
	ctx := r.Context()
	err = h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		before, err := q.GetCategoryAttribute(ctx, dbq.GetCategoryAttributeParams{ID: int32(attrID), CategoryID: int32(id)})
		if err != nil {
			return notFound(err, "attribute")
		}
		n, err := q.CountItemsWithAttribute(ctx, dbq.CountItemsWithAttributeParams{CategoryID: int32(id), Code: before.Code})
		if err != nil {
			return err
		}
		if n > 0 {
			return httpx.InvalidState("items use this attribute; it cannot be removed")
		}
		if _, err := q.DeleteCategoryAttribute(ctx, dbq.DeleteCategoryAttributeParams{ID: int32(attrID), CategoryID: int32(id)}); err != nil {
			return err
		}
		return audit.Log(ctx, q, "delete", "category_attribute", audit.ID(attrID), toAttribute(before), nil)
	})
	if err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}

// ---------------------------------------------------------------- helpers

func notFound(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return httpx.NotFound(what)
	}
	return err
}

func field(list string, i int, name string) string {
	f := list + "[" + itoa(i) + "]"
	if name != "" {
		f += "." + name
	}
	return f
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return itoa(i/10) + string(rune('0'+i%10))
}

// prefix adds a path prefix to a validation problem's field names.
func prefix(err error, p string) error {
	var pr *httpx.Problem
	if errors.As(err, &pr) {
		for i := range pr.Errors {
			pr.Errors[i].Field = p + "." + pr.Errors[i].Field
		}
	}
	return err
}
