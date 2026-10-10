// Package ledger handles parties (customers and dealers), their ledgers and
// statements, and payments received and paid (FR-CU, FR-PU-03).
//
// Ledger sign: debit = the party owes the shop more. Receipts credit the
// customer; payments to dealers debit the dealer.
package ledger

import (
	"context"
	"errors"
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
	"billing-api/internal/num"
)

type handler struct{ a *app.App }

func PartyRoutes(a *app.App) func(chi.Router) {
	h := handler{a}
	return func(r chi.Router) {
		r.Get("/", httpx.H(h.list))
		r.Post("/", httpx.H(h.create))
		r.Get("/{id}", httpx.H(h.get))
		r.Patch("/{id}", httpx.H(h.update))
		r.With(auth.SuperAdmin).Post("/{id}/deactivate", httpx.H(h.deactivate))
		r.Get("/{id}/ledger", httpx.H(h.ledger))
		r.Get("/{id}/statement", httpx.H(h.statement))
	}
}

// LoadParties returns API parties for ids, in order.
func LoadParties(ctx context.Context, q *dbq.Queries, ids []int64) ([]api.Party, error) {
	if len(ids) == 0 {
		return []api.Party{}, nil
	}
	rows, err := q.PartiesByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	sites, err := q.SitesByParties(ctx, ids)
	if err != nil {
		return nil, err
	}
	bySites := map[int64][]api.PartySite{}
	for _, s := range sites {
		bySites[s.PartyID] = append(bySites[s.PartyID], api.PartySite{ID: s.ID, Label: s.Label, Address: s.Address})
	}
	byID := map[int64]api.Party{}
	for _, p := range rows {
		x := api.Party{ID: p.ID, Kind: string(p.Kind), Name: p.Name, Mobile: p.Mobile, AltPhone: p.AltPhone,
			GSTIN: trimPtr(p.Gstin), StateCode: trimPtr(p.StateCode), BillingAddress: p.BillingAddress,
			Pincode: trimPtr(p.Pincode), CreditLimit: api.MP(p.CreditLimit), OpeningBalance: api.M(p.OpeningBalance),
			Balance: api.M(p.Balance), Sites: bySites[p.ID], Notes: p.Notes, IsActive: p.IsActive}
		if x.Sites == nil {
			x.Sites = []api.PartySite{}
		}
		byID[p.ID] = x
	}
	out := make([]api.Party, 0, len(ids))
	for _, id := range ids {
		if p, ok := byID[id]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func trimPtr(s *string) *string {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	return &t
}

func GetParty(ctx context.Context, q *dbq.Queries, id int64) (api.Party, error) {
	ps, err := LoadParties(ctx, q, []int64{id})
	if err != nil {
		return api.Party{}, err
	}
	if len(ps) == 0 {
		return api.Party{}, httpx.NotFound("party")
	}
	return ps[0], nil
}

func (h handler) list(w http.ResponseWriter, r *http.Request) error {
	page, err := httpx.ParsePage(r)
	if err != nil {
		return err
	}
	kind, err := httpx.QEnum(r, "kind", "customer", "supplier", "both")
	if err != nil {
		return err
	}
	bal, err := httpx.QEnum(r, "with_balance", "any", "receivable", "payable")
	if err != nil {
		return err
	}
	inactive, err := httpx.QBool(r, "include_inactive")
	if err != nil {
		return err
	}
	sort, desc, err := httpx.Sort(r, "name", "name", "balance")
	if err != nil {
		return err
	}
	rows, err := h.a.Q.ListPartyIDs(r.Context(), dbq.ListPartyIDsParams{IncludeInactive: inactive, Kind: kind,
		Q: httpx.QString(r, "q"), WithBalance: bal, Sort: sort, Descending: desc, Lim: page.Limit(), Off: page.Offset()})
	if err != nil {
		return err
	}
	ids := make([]int64, len(rows))
	var total int64
	for i, row := range rows {
		ids[i], total = row.ID, row.Total
	}
	out, err := LoadParties(r.Context(), h.a.Q, ids)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, httpx.NewList(out, page, total))
	return nil
}

func (h handler) get(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	p, err := GetParty(r.Context(), h.a.Q, id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, p)
	return nil
}

type partyFields struct {
	gstin, state *string
	credit       *decimal.Decimal
	opening      decimal.Decimal
	openingDate  *time.Time
}

// checkParty validates GSTIN against the state code (first 2 digits).
func checkParty(in api.PartyInput) (partyFields, error) {
	f := partyFields{credit: num.ParsePtr(in.CreditLimit), openingDate: httpx.ParseDate(in.OpeningDate)}
	if in.OpeningBalance != nil {
		f.opening = num.Parse(*in.OpeningBalance)
	}
	if in.GSTIN != nil && *in.GSTIN != "" {
		g := strings.ToUpper(*in.GSTIN)
		f.gstin = &g
		st := g[:2]
		if in.StateCode != nil && *in.StateCode != "" && *in.StateCode != st {
			return f, httpx.Validation("state_code", "must match the first two digits of the GSTIN ("+st+")")
		}
		f.state = &st
	} else if in.StateCode != nil && *in.StateCode != "" {
		f.state = in.StateCode
	}
	return f, nil
}

func blank(s *string) *string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil
	}
	t := strings.TrimSpace(*s)
	return &t
}

func (h handler) create(w http.ResponseWriter, r *http.Request) error {
	var in api.PartyInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	f, err := checkParty(in)
	if err != nil {
		return err
	}
	ctx := r.Context()
	var out api.Party
	err = h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		id, err := q.InsertParty(ctx, dbq.InsertPartyParams{Kind: dbq.PartyKind(in.Kind), Name: strings.TrimSpace(in.Name),
			Mobile: blank(in.Mobile), AltPhone: blank(in.AltPhone), Gstin: f.gstin, StateCode: f.state,
			BillingAddress: blank(in.BillingAddress), Pincode: blank(in.Pincode), CreditLimit: f.credit,
			OpeningBalance: f.opening, OpeningDate: f.openingDate, Notes: blank(in.Notes)})
		if err != nil {
			return err
		}
		for _, s := range in.Sites {
			if err := q.InsertPartySite(ctx, dbq.InsertPartySiteParams{PartyID: id, Label: s.Label, Address: s.Address}); err != nil {
				return err
			}
		}
		if out, err = GetParty(ctx, q, id); err != nil {
			return err
		}
		return audit.Log(ctx, q, "create", "party", audit.ID(id), nil, out)
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusCreated, out)
	return nil
}

// update replaces the party's fields; sites are replaced when given.
func (h handler) update(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in api.PartyInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	f, err := checkParty(in)
	if err != nil {
		return err
	}
	ctx := r.Context()
	var out api.Party
	err = h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		before, err := GetParty(ctx, q, id)
		if err != nil {
			return err
		}
		if in.OpeningBalance == nil {
			f.opening = before.OpeningBalance.D()
		}
		if _, err := q.UpdateParty(ctx, dbq.UpdatePartyParams{ID: id, Kind: dbq.PartyKind(in.Kind), Name: strings.TrimSpace(in.Name),
			Mobile: blank(in.Mobile), AltPhone: blank(in.AltPhone), Gstin: f.gstin, StateCode: f.state,
			BillingAddress: blank(in.BillingAddress), Pincode: blank(in.Pincode), CreditLimit: f.credit,
			OpeningBalance: f.opening, OpeningDate: f.openingDate, Notes: blank(in.Notes)}); err != nil {
			return err
		}
		if in.Sites != nil {
			if err := q.DeletePartySites(ctx, id); err != nil {
				return err
			}
			for _, s := range in.Sites {
				if err := q.InsertPartySite(ctx, dbq.InsertPartySiteParams{PartyID: id, Label: s.Label, Address: s.Address}); err != nil {
					return err
				}
			}
		}
		if out, err = GetParty(ctx, q, id); err != nil {
			return err
		}
		return audit.Log(ctx, q, "update", "party", audit.ID(id), before, out)
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, out)
	return nil
}

func (h handler) deactivate(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	err = h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		n, err := q.SetPartyActive(ctx, dbq.SetPartyActiveParams{ID: id, IsActive: false})
		if err != nil {
			return err
		}
		if n == 0 {
			return httpx.NotFound("party")
		}
		return audit.Log(ctx, q, "deactivate", "party", audit.ID(id), nil, nil)
	})
	if err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}

// BuildLedger returns opening, entries with running balance and closing
// for a date range (FR-CU-02).
func BuildLedger(ctx context.Context, q *dbq.Queries, id int64, from, to *time.Time) (api.Ledger, error) {
	p, err := GetParty(ctx, q, id)
	if err != nil {
		return api.Ledger{}, err
	}
	opening, err := q.PartyLedgerOpening(ctx, dbq.PartyLedgerOpeningParams{PartyID: id, FromDate: from})
	if err != nil {
		return api.Ledger{}, err
	}
	rows, err := q.PartyLedgerEntries(ctx, dbq.PartyLedgerEntriesParams{PartyID: id, FromDate: from, ToDate: to})
	if err != nil {
		return api.Ledger{}, err
	}
	bal := opening
	out := api.Ledger{Party: p, Opening: api.M(opening), Entries: make([]api.LedgerEntry, len(rows))}
	for i, e := range rows {
		bal = bal.Add(e.Debit).Sub(e.Credit)
		out.Entries[i] = api.LedgerEntry{ID: e.ID, Date: api.Date(e.EntryDate), RefType: string(e.RefType), RefID: e.RefID,
			RefNo: e.RefNo, Narration: e.Narration, Debit: api.M(e.Debit), Credit: api.M(e.Credit), Balance: api.M(bal)}
	}
	out.Closing = api.M(bal)
	return out, nil
}

func (h handler) ledger(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	from, to, err := httpx.Range(r)
	if err != nil {
		return err
	}
	l, err := BuildLedger(r.Context(), h.a.Q, id, from, to)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, l)
	return nil
}

func (h handler) statement(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	from, to, err := httpx.Range(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	l, err := BuildLedger(ctx, h.a.Q, id, from, to)
	if err != nil {
		return err
	}
	s, err := h.a.Q.GetSettings(ctx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	pdf, err := docs.Statement(s, l, from, to)
	if err != nil {
		return err
	}
	link, err := docs.Publish(ctx, h.a, "statements/party-"+itoa(id)+".pdf", pdf)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, link)
	return nil
}

func itoa(n int64) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
