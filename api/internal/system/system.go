// Package system serves search, lookups, notifications and the audit log.
package system

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"billing-api/internal/api"
	"billing-api/internal/app"
	"billing-api/internal/auth"
	"billing-api/internal/db/dbq"
	"billing-api/internal/httpx"
	"billing-api/internal/inventory"
)

type handler struct{ a *app.App }

// States are the GST state codes (CBIC list).
var States = []api.State{
	{Code: "01", Name: "Jammu and Kashmir"}, {Code: "02", Name: "Himachal Pradesh"}, {Code: "03", Name: "Punjab"}, {Code: "04", Name: "Chandigarh"},
	{Code: "05", Name: "Uttarakhand"}, {Code: "06", Name: "Haryana"}, {Code: "07", Name: "Delhi"}, {Code: "08", Name: "Rajasthan"}, {Code: "09", Name: "Uttar Pradesh"},
	{Code: "10", Name: "Bihar"}, {Code: "11", Name: "Sikkim"}, {Code: "12", Name: "Arunachal Pradesh"}, {Code: "13", Name: "Nagaland"}, {Code: "14", Name: "Manipur"},
	{Code: "15", Name: "Mizoram"}, {Code: "16", Name: "Tripura"}, {Code: "17", Name: "Meghalaya"}, {Code: "18", Name: "Assam"}, {Code: "19", Name: "West Bengal"},
	{Code: "20", Name: "Jharkhand"}, {Code: "21", Name: "Odisha"}, {Code: "22", Name: "Chhattisgarh"}, {Code: "23", Name: "Madhya Pradesh"}, {Code: "24", Name: "Gujarat"},
	{Code: "26", Name: "Dadra and Nagar Haveli and Daman and Diu"}, {Code: "27", Name: "Maharashtra"}, {Code: "29", Name: "Karnataka"}, {Code: "30", Name: "Goa"},
	{Code: "31", Name: "Lakshadweep"}, {Code: "32", Name: "Kerala"}, {Code: "33", Name: "Tamil Nadu"}, {Code: "34", Name: "Puducherry"},
	{Code: "35", Name: "Andaman and Nicobar Islands"}, {Code: "36", Name: "Telangana"}, {Code: "37", Name: "Andhra Pradesh"}, {Code: "38", Name: "Ladakh"},
	{Code: "97", Name: "Other Territory"}, {Code: "99", Name: "Centre Jurisdiction"},
}

func (h handler) states(w http.ResponseWriter, _ *http.Request) error {
	httpx.JSON(w, http.StatusOK, States)
	return nil
}

// SearchRoute is GET /search (command palette).
func SearchRoute(a *app.App) http.HandlerFunc { return httpx.H(handler{a}.search) }

// StatesRoute is GET /lookups/states.
func StatesRoute(a *app.App) http.HandlerFunc { return httpx.H(handler{a}.states) }

func (h handler) search(w http.ResponseWriter, r *http.Request) error {
	q := httpx.QString(r, "q")
	out := api.SearchResult{Items: []api.SearchItem{}, Parties: []api.SearchParty{}, Bills: []api.SearchBill{}}
	if len(q) < 2 {
		httpx.JSON(w, http.StatusOK, out)
		return nil
	}
	ctx := r.Context()
	items, err := h.a.Q.SearchItems(ctx, inventory.Terms(q))
	if err != nil {
		return err
	}
	for _, it := range items {
		out.Items = append(out.Items, api.SearchItem{ID: it.ID, Name: it.Name, StockTotal: api.Q(it.StockTotal)})
	}
	parties, err := h.a.Q.SearchParties(ctx, q)
	if err != nil {
		return err
	}
	for _, p := range parties {
		out.Parties = append(out.Parties, api.SearchParty{ID: p.ID, Name: p.Name, Mobile: p.Mobile})
	}
	bills, err := h.a.Q.SearchBills(ctx, q)
	if err != nil {
		return err
	}
	for _, b := range bills {
		out.Bills = append(out.Bills, api.SearchBill{ID: b.ID, BillNo: b.BillNo, BillType: string(b.BillType),
			CustomerName: b.CustName, BillDate: api.Date(b.BillDate)})
	}
	httpx.JSON(w, http.StatusOK, out)
	return nil
}

// NotificationRoutes mounts /notifications (role broadcasts + personal).
func NotificationRoutes(a *app.App) func(chi.Router) {
	h := handler{a}
	return func(r chi.Router) {
		r.Get("/", httpx.H(h.notifications))
		r.Post("/read-all", httpx.H(h.readAll))
		r.Post("/{id}/read", httpx.H(h.read))
	}
}

var notificationKinds = map[string]bool{"stock_short": true, "low_stock": true, "return_pending": true,
	"einv_failed": true, "backup_failed": true}

func (h handler) notifications(w http.ResponseWriter, r *http.Request) error {
	page, err := httpx.ParsePage(r)
	if err != nil {
		return err
	}
	unread, err := httpx.QBool(r, "unread_only")
	if err != nil {
		return err
	}
	u := auth.MustUser(r)
	rows, err := h.a.Q.ListNotifications(r.Context(), dbq.ListNotificationsParams{UserID: &u.ID, Role: dbq.UserRole(u.Role),
		UnreadOnly: unread, Lim: page.Limit(), Off: page.Offset()})
	if err != nil {
		return err
	}
	out := make([]api.Notification, len(rows))
	var total int64
	for i, n := range rows {
		total = n.Total
		kind := n.Kind
		if !notificationKinds[kind] {
			kind = "other"
		}
		x := api.Notification{ID: n.ID, Kind: kind, Title: n.Title, Body: n.Body, RefID: n.RefID, ItemID: n.ItemID,
			IsRead: n.IsRead, CreatedAt: n.CreatedAt}
		if n.RefType != nil {
			s := string(*n.RefType)
			x.RefType = &s
		}
		out[i] = x
	}
	httpx.JSON(w, http.StatusOK, httpx.NewList(out, page, total))
	return nil
}

func (h handler) read(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	u := auth.MustUser(r)
	n, err := h.a.Q.MarkNotificationRead(r.Context(), dbq.MarkNotificationReadParams{ID: id, UserID: &u.ID, Role: dbq.UserRole(u.Role)})
	if err != nil {
		return err
	}
	if n == 0 {
		return httpx.NotFound("notification")
	}
	httpx.NoContent(w)
	return nil
}

func (h handler) readAll(w http.ResponseWriter, r *http.Request) error {
	u := auth.MustUser(r)
	if err := h.a.Q.MarkAllNotificationsRead(r.Context(), dbq.MarkAllNotificationsReadParams{UserID: &u.ID, Role: dbq.UserRole(u.Role)}); err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}

// AuditRoutes mounts GET /audit-log (Super Admin).
func AuditRoutes(a *app.App) func(chi.Router) {
	h := handler{a}
	return func(r chi.Router) {
		r.Use(auth.SuperAdmin)
		r.Get("/", httpx.H(h.audit))
	}
}

func (h handler) audit(w http.ResponseWriter, r *http.Request) error {
	page, err := httpx.ParsePage(r)
	if err != nil {
		return err
	}
	p := dbq.ListAuditParams{Lim: page.Limit(), Off: page.Offset()}
	if v := r.URL.Query().Get("user_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return httpx.Validation("user_id", "must be a UUID")
		}
		p.UserID = &id
	}
	if v := httpx.QString(r, "entity"); v != "" {
		p.Entity = &v
	}
	if v := httpx.QString(r, "action"); v != "" {
		p.Action = &v
	}
	if p.EntityID, err = httpx.QInt64(r, "entity_id"); err != nil {
		return err
	}
	if p.FromDate, p.ToDate, err = httpx.Range(r); err != nil {
		return err
	}
	rows, err := h.a.Q.ListAudit(r.Context(), p)
	if err != nil {
		return err
	}
	out := make([]api.AuditEntry, len(rows))
	var total int64
	for i, a := range rows {
		total = a.Total
		e := api.AuditEntry{ID: a.ID, UserName: a.UserName, Action: a.Action, Entity: a.Entity, EntityID: a.EntityID,
			Before: raw(a.BeforeData), After: raw(a.AfterData), CreatedAt: a.CreatedAt}
		if a.IpAddress != "" {
			ip := a.IpAddress
			e.IPAddress = &ip
		}
		out[i] = e
	}
	httpx.JSON(w, http.StatusOK, httpx.NewList(out, page, total))
	return nil
}

// raw keeps objects as-is; other JSON (arrays, scalars) is wrapped so the
// field is always an object or null.
func raw(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	if b[0] == '{' {
		return b
	}
	w, _ := json.Marshal(map[string]json.RawMessage{"value": b})
	return w
}
