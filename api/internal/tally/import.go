package tally

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/shopspring/decimal"
	"github.com/xuri/excelize/v2"

	"billing-api/internal/api"
	"billing-api/internal/app"
	"billing-api/internal/audit"
	"billing-api/internal/auth"
	"billing-api/internal/db/dbq"
	"billing-api/internal/httpx"
	"billing-api/internal/jobs"
	"billing-api/internal/system"
)

// importData is the normalised content of an uploaded file.
type importData struct {
	Groups    []string   `json:"groups"`
	Units     []impUnit  `json:"units"`
	Items     []impItem  `json:"items"`
	Customers []impParty `json:"customers"`
	Suppliers []impParty `json:"suppliers"`
}

type impUnit struct {
	Name     string `json:"name"`
	Decimals int    `json:"decimals"`
}

type impOpening struct {
	Godown string `json:"godown"`
	Qty    string `json:"qty"`
	Rate   string `json:"rate"`
}

type impItem struct {
	Name    string       `json:"name"`
	Group   string       `json:"group"`
	Unit    string       `json:"unit"`
	HSN     string       `json:"hsn"`
	GST     string       `json:"gst"`
	Opening []impOpening `json:"opening"`
}

// Opening uses the app's sign: positive = the party owes the shop.
type impParty struct {
	Name    string `json:"name"`
	GSTIN   string `json:"gstin"`
	State   string `json:"state"`
	Address string `json:"address"`
	Mobile  string `json:"mobile"`
	Opening string `json:"opening"`
}

var allScopes = []string{"groups", "units", "items", "customers", "suppliers", "opening_stock", "opening_balances"}

var reNum = regexp.MustCompile(`-?[0-9][0-9,]*(\.[0-9]+)?`)

// leadingNumber reads "100.000 kg" or "350.00/kg" -> "100.000" / "350.00".
func leadingNumber(s string) string {
	m := reNum.FindString(strings.TrimSpace(s))
	return strings.ReplaceAll(m, ",", "")
}

// ---------------------------------------------------------------- Tally XML

type tEnvelope struct {
	Messages []tMessage `xml:"BODY>IMPORTDATA>REQUESTDATA>TALLYMESSAGE"`
}

type tMessage struct {
	Groups  []tNamed  `xml:"STOCKGROUP"`
	Units   []tUnit   `xml:"UNIT"`
	Items   []tItem   `xml:"STOCKITEM"`
	Ledgers []tLedger `xml:"LEDGER"`
}

type tNamed struct {
	Name string `xml:"NAME,attr"`
}

type tUnit struct {
	Name     string `xml:"NAME,attr"`
	Decimals string `xml:"DECIMALPLACES"`
}

type tItem struct {
	Name     string   `xml:"NAME,attr"`
	Parent   string   `xml:"PARENT"`
	Base     string   `xml:"BASEUNITS"`
	Opening  string   `xml:"OPENINGBALANCE"`
	Rate     string   `xml:"OPENINGRATE"`
	HSN      string   `xml:"GSTDETAILS.LIST>HSNCODE"`
	GSTRates []string `xml:"GSTDETAILS.LIST>STATEWISEDETAILS.LIST>RATEDETAILS.LIST>GSTRATE"`
	Batches  []struct {
		Godown  string `xml:"GODOWNNAME"`
		Opening string `xml:"OPENINGBALANCE"`
		Rate    string `xml:"OPENINGRATE"`
	} `xml:"BATCHALLOCATIONS.LIST"`
}

type tLedger struct {
	Name    string   `xml:"NAME,attr"`
	Parent  string   `xml:"PARENT"`
	Opening string   `xml:"OPENINGBALANCE"`
	GSTIN   string   `xml:"PARTYGSTIN"`
	State   string   `xml:"LEDSTATENAME"`
	Address []string `xml:"ADDRESS.LIST>ADDRESS"`
	Mobile  string   `xml:"LEDGERMOBILE"`
}

func parseTallyXML(b []byte) (importData, error) {
	var env tEnvelope
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = false
	if err := dec.Decode(&env); err != nil {
		return importData{}, fmt.Errorf("not a Tally XML export: %w", err)
	}
	var d importData
	for _, m := range env.Messages {
		for _, g := range m.Groups {
			d.Groups = append(d.Groups, strings.TrimSpace(g.Name))
		}
		for _, u := range m.Units {
			dp := 0
			fmt.Sscanf(strings.TrimSpace(u.Decimals), "%d", &dp)
			d.Units = append(d.Units, impUnit{Name: strings.TrimSpace(u.Name), Decimals: dp})
		}
		for _, it := range m.Items {
			x := impItem{Name: strings.TrimSpace(it.Name), Group: strings.TrimSpace(it.Parent), Unit: strings.TrimSpace(it.Base),
				HSN: strings.TrimSpace(it.HSN)}
			best := decimal.Zero
			for _, r := range it.GSTRates {
				if v, err := decimal.NewFromString(leadingNumber(r)); err == nil && v.GreaterThan(best) {
					best = v
				}
			}
			if best.IsPositive() {
				x.GST = best.String()
			}
			if len(it.Batches) > 0 {
				for _, bt := range it.Batches {
					if q := leadingNumber(bt.Opening); q != "" && q != "0" {
						x.Opening = append(x.Opening, impOpening{Godown: strings.TrimSpace(bt.Godown), Qty: q, Rate: leadingNumber(bt.Rate)})
					}
				}
			} else if q := leadingNumber(it.Opening); q != "" && q != "0" {
				x.Opening = append(x.Opening, impOpening{Qty: q, Rate: leadingNumber(it.Rate)})
			}
			d.Items = append(d.Items, x)
		}
		for _, l := range m.Ledgers {
			p := impParty{Name: strings.TrimSpace(l.Name), GSTIN: strings.ToUpper(strings.TrimSpace(l.GSTIN)),
				State: stateCode(l.State), Address: strings.TrimSpace(strings.Join(l.Address, ", ")),
				Mobile: strings.TrimSpace(l.Mobile)}
			// Tally: debit balances are negative; the app uses + for "owes the shop".
			if v, err := decimal.NewFromString(leadingNumber(l.Opening)); err == nil {
				p.Opening = v.Neg().StringFixed(2)
			}
			switch strings.ToLower(strings.TrimSpace(l.Parent)) {
			case "sundry debtors":
				d.Customers = append(d.Customers, p)
			case "sundry creditors":
				d.Suppliers = append(d.Suppliers, p)
			}
		}
	}
	return d, nil
}

// ---------------------------------------------------------------- Excel

// parseExcel reads the import workbook: sheets Groups, Units, Items,
// Customers and Suppliers with a header row (see README). Opening balances
// are positive when the amount is due (customers owe us / we owe suppliers).
func parseExcel(b []byte) (importData, error) {
	f, err := excelize.OpenReader(bytes.NewReader(b))
	if err != nil {
		return importData{}, fmt.Errorf("not an .xlsx file: %w", err)
	}
	defer f.Close()
	sheet := func(name string) []map[string]string {
		rows, err := f.GetRows(name)
		if err != nil || len(rows) < 2 {
			return nil
		}
		var out []map[string]string
		for _, r := range rows[1:] {
			m := map[string]string{}
			for i, h := range rows[0] {
				if i < len(r) {
					m[strings.ToLower(strings.TrimSpace(h))] = strings.TrimSpace(r[i])
				}
			}
			if m["name"] != "" {
				out = append(out, m)
			}
		}
		return out
	}
	var d importData
	for _, r := range sheet("Groups") {
		d.Groups = append(d.Groups, r["name"])
	}
	for _, r := range sheet("Units") {
		dp := 0
		fmt.Sscanf(r["decimals"], "%d", &dp)
		d.Units = append(d.Units, impUnit{Name: r["name"], Decimals: dp})
	}
	for _, r := range sheet("Items") {
		x := impItem{Name: r["name"], Group: r["group"], Unit: r["unit"], HSN: r["hsn"], GST: strings.TrimSuffix(r["gst rate"], "%")}
		if q := leadingNumber(r["opening qty"]); q != "" && q != "0" {
			x.Opening = append(x.Opening, impOpening{Godown: r["godown"], Qty: q, Rate: leadingNumber(r["opening rate"])})
		}
		d.Items = append(d.Items, x)
	}
	party := func(r map[string]string, negate bool) impParty {
		p := impParty{Name: r["name"], GSTIN: strings.ToUpper(r["gstin"]), State: r["state code"], Address: r["address"], Mobile: r["mobile"]}
		if v, err := decimal.NewFromString(leadingNumber(r["opening balance"])); err == nil {
			if negate {
				v = v.Neg()
			}
			p.Opening = v.StringFixed(2)
		}
		return p
	}
	for _, r := range sheet("Customers") {
		d.Customers = append(d.Customers, party(r, false))
	}
	for _, r := range sheet("Suppliers") {
		d.Suppliers = append(d.Suppliers, party(r, true))
	}
	return d, nil
}

func stateCode(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, s := range system.States {
		if strings.ToLower(s.Name) == n {
			return s.Code
		}
	}
	return ""
}

func stateName(code string) string {
	for _, s := range system.States {
		if s.Code == code {
			return s.Name
		}
	}
	return ""
}

var reGSTIN = regexp.MustCompile(`^[0-9]{2}[A-Z]{5}[0-9]{4}[A-Z][0-9A-Z]Z[0-9A-Z]$`)

// ---------------------------------------------------------------- preview

func (h handler) preview(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, 20<<20)
	if err := r.ParseMultipartForm(20 << 20); err != nil {
		return httpx.Validation("file", "upload the Tally XML or Excel file (max 20 MB) as multipart field \"file\"")
	}
	cutover := httpx.ParseDate(ptrStr(r.FormValue("cutover_date")))
	if cutover == nil {
		return httpx.Validation("cutover_date", "is required (YYYY-MM-DD)")
	}
	scope := r.MultipartForm.Value["scope"]
	if len(scope) == 0 {
		scope = r.MultipartForm.Value["scope[]"]
	}
	if len(scope) == 1 && strings.Contains(scope[0], ",") {
		scope = strings.Split(scope[0], ",")
	}
	if len(scope) == 0 {
		scope = allScopes
	}
	for _, s := range scope {
		if !contains(allScopes, s) {
			return httpx.Validation("scope", "unknown scope "+s)
		}
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		return httpx.Validation("file", "is required")
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	source := "tally_xml"
	var d importData
	if bytes.HasPrefix(data, []byte("PK")) {
		source = "excel"
		d, err = parseExcel(data)
	} else {
		d, err = parseTallyXML(data)
	}
	if err != nil {
		return httpx.Validation("file", err.Error())
	}
	ctx := r.Context()
	counts, issues, err := analyse(ctx, h.a.Q, d, scope)
	if err != nil {
		return err
	}
	dataJSON, _ := json.Marshal(d)
	countsJSON, _ := json.Marshal(counts)
	issuesJSON, _ := json.Marshal(issues)
	expires := h.a.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	id, err := h.a.Q.InsertTallyImport(ctx, dbq.InsertTallyImportParams{Source: source, CutoverDate: *cutover, Scope: scope,
		Data: dataJSON, Counts: countsJSON, Issues: issuesJSON, ExpiresAt: expires, CreatedBy: auth.UserID(ctx)})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusCreated, api.TallyImportPreview{ImportID: id, Source: source, CutoverDate: api.Date(*cutover),
		Counts: counts, Issues: issues, ExpiresAt: expires})
	return nil
}

func ptrStr(s string) *string { return &s }

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// analyse counts new/existing records and lists problems to fix before commit.
func analyse(ctx context.Context, q *dbq.Queries, d importData, scope []string) (map[string]api.ImportCount, []api.ImportIssue, error) {
	counts := map[string]api.ImportCount{}
	issues := []api.ImportIssue{}
	exists := func(fn func(context.Context, string) (int64, error), name string) (bool, error) {
		_, err := fn(ctx, name)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}
	cat := func(ctx context.Context, n string) (int64, error) {
		id, err := q.CategoryByName(ctx, n)
		return int64(id), err
	}
	unit := func(ctx context.Context, n string) (int64, error) {
		id, err := q.UnitByName(ctx, n)
		return int64(id), err
	}
	tally := func(key, name string, fn func(context.Context, string) (int64, error)) error {
		c := counts[key]
		ok, err := exists(fn, name)
		if err != nil {
			return err
		}
		if ok {
			c.Existing++
		} else {
			c.New++
		}
		counts[key] = c
		return nil
	}
	if contains(scope, "groups") {
		for _, g := range d.Groups {
			if err := tally("groups", g, cat); err != nil {
				return nil, nil, err
			}
		}
	}
	importedUnits := map[string]bool{}
	if contains(scope, "units") {
		for _, u := range d.Units {
			importedUnits[strings.ToLower(u.Name)] = true
			if err := tally("units", u.Name, unit); err != nil {
				return nil, nil, err
			}
		}
	}
	if contains(scope, "items") {
		for _, it := range d.Items {
			if err := tally("items", it.Name, q.ItemIDByName); err != nil {
				return nil, nil, err
			}
			if ok, _ := exists(unit, it.Unit); !ok && !importedUnits[strings.ToLower(it.Unit)] {
				c := counts["items"]
				c.Errors++
				counts["items"] = c
				issues = append(issues, api.ImportIssue{Entity: "item", Name: it.Name, Problem: "unknown unit " + strconvQuote(it.Unit),
					Suggestion: "include units in the scope or map it in unit_map when committing"})
			}
			if it.GST != "" && !httpx.GSTRates[decimalTrim(it.GST)] {
				issues = append(issues, api.ImportIssue{Entity: "item", Name: it.Name, Problem: "GST rate " + it.GST + " is not allowed",
					Suggestion: "it will be imported at 18%; correct it afterwards"})
			}
		}
	}
	if contains(scope, "opening_stock") {
		c := api.ImportCount{}
		for _, it := range d.Items {
			for _, op := range it.Opening {
				c.New++
				if op.Godown != "" {
					if ok, _ := exists(func(ctx context.Context, n string) (int64, error) {
						id, err := q.LocationByName(ctx, n)
						return int64(id), err
					}, op.Godown); !ok {
						issues = append(issues, api.ImportIssue{Entity: "opening_stock", Name: it.Name,
							Problem: "unknown godown " + strconvQuote(op.Godown), Suggestion: "map it in location_map when committing"})
					}
				}
			}
		}
		counts["opening_stock"] = c
	}
	for _, kind := range []string{"customers", "suppliers"} {
		if !contains(scope, kind) {
			continue
		}
		list := d.Customers
		if kind == "suppliers" {
			list = d.Suppliers
		}
		for _, p := range list {
			if err := tally(kind, p.Name, q.PartyIDByName); err != nil {
				return nil, nil, err
			}
			if p.GSTIN != "" && !reGSTIN.MatchString(p.GSTIN) {
				issues = append(issues, api.ImportIssue{Entity: kind, Name: p.Name, Problem: "invalid GSTIN " + p.GSTIN,
					Suggestion: "it will be left empty"})
			}
		}
	}
	if contains(scope, "opening_balances") {
		c := api.ImportCount{}
		for _, p := range append(append([]impParty{}, d.Customers...), d.Suppliers...) {
			if p.Opening != "" && p.Opening != "0.00" {
				c.New++
			}
		}
		counts["opening_balances"] = c
	}
	return counts, issues, nil
}

func strconvQuote(s string) string { return `"` + s + `"` }

func decimalTrim(s string) string {
	d, err := decimal.NewFromString(strings.TrimSpace(s))
	if err != nil {
		return s
	}
	return d.String()
}

// ---------------------------------------------------------------- commit

type ImportArgs struct {
	JobID uuid.UUID `json:"job_id"`
}

func (ImportArgs) Kind() string { return "tally_import" }

type commitParams struct {
	ImportID    int64            `json:"import_id"`
	LocationMap map[string]int64 `json:"location_map"`
	UnitMap     map[string]int64 `json:"unit_map"`
	UserID      uuid.UUID        `json:"user_id"`
}

func (h handler) commit(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in api.TallyCommitInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	var job dbq.Job
	err = h.a.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		imp, err := q.LockTallyImport(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.NotFound("import")
		}
		if err != nil {
			return err
		}
		if imp.Status != "preview" {
			return httpx.InvalidState("this import is already " + imp.Status)
		}
		if h.a.Now().After(imp.ExpiresAt) {
			return httpx.InvalidState("the preview expired; upload the file again")
		}
		job, err = jobs.Create(ctx, q, "tally_import", commitParams{ImportID: id, LocationMap: in.LocationMap,
			UnitMap: in.UnitMap, UserID: auth.MustUser(r).ID})
		if err != nil {
			return err
		}
		if err := q.SetTallyImportStatus(ctx, dbq.SetTallyImportStatusParams{ID: id, Status: "committing", JobID: &job.ID}); err != nil {
			return err
		}
		_, err = h.a.River.InsertTx(ctx, tx, ImportArgs{JobID: job.ID}, nil)
		return err
	})
	if err != nil {
		return err
	}
	jobs.Accepted(w, r, h.a, job)
	return nil
}

type importWorker struct {
	river.WorkerDefaults[ImportArgs]
	a *app.App
}

func (wk *importWorker) Work(ctx context.Context, job *river.Job[ImportArgs]) error {
	a := wk.a
	var p commitParams
	return jobs.Track(ctx, a, job.Args.JobID, func(ctx context.Context) (jobs.Outcome, error) {
		j, err := a.Q.GetJob(ctx, job.Args.JobID)
		if err != nil {
			return jobs.Outcome{}, err
		}
		if err := json.Unmarshal(j.Params, &p); err != nil {
			return jobs.Outcome{}, err
		}
		if u, err := a.Q.GetAppUser(ctx, p.UserID); err == nil {
			ctx = auth.WithUser(ctx, auth.User{ID: u.ID, FullName: u.FullName, Role: string(u.Role), Row: u})
		}
		var result map[string]int
		err = a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
			var err error
			result, err = apply(ctx, a, q, p)
			return err
		})
		if err != nil {
			_ = a.Q.SetTallyImportStatus(ctx, dbq.SetTallyImportStatusParams{ID: p.ImportID, Status: "failed"})
			return jobs.Outcome{}, err
		}
		return jobs.Outcome{Result: result}, nil
	})
}

// apply writes the import in one transaction (all-or-nothing).
func apply(ctx context.Context, a *app.App, q *dbq.Queries, p commitParams) (map[string]int, error) {
	imp, err := q.LockTallyImport(ctx, p.ImportID)
	if err != nil {
		return nil, err
	}
	var d importData
	if err := json.Unmarshal(imp.Data, &d); err != nil {
		return nil, err
	}
	scope := imp.Scope
	res := map[string]int{}
	user := auth.UserID(ctx)

	catID := func(name string) (int32, error) {
		if name == "" {
			name = "Imported"
		}
		id, err := q.CategoryByName(ctx, name)
		if errors.Is(err, pgx.ErrNoRows) {
			res["groups"]++
			return q.InsertImportedCategory(ctx, name)
		}
		return id, err
	}
	if contains(scope, "groups") {
		for _, g := range d.Groups {
			if _, err := catID(g); err != nil {
				return nil, err
			}
		}
	}
	if contains(scope, "units") {
		for _, u := range d.Units {
			if _, err := q.UnitByName(ctx, u.Name); errors.Is(err, pgx.ErrNoRows) {
				dec := int16(min(max(u.Decimals, 0), 3))
				if _, err := q.InsertUnit(ctx, dbq.InsertUnitParams{Name: u.Name, Symbol: u.Name, Decimals: dec}); err != nil {
					return nil, err
				}
				res["units"]++
			} else if err != nil {
				return nil, err
			}
		}
	}
	unitID := func(name string) (int32, error) {
		if id, ok := p.UnitMap[name]; ok {
			return int32(id), nil
		}
		id, err := q.UnitByName(ctx, name)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("unit %q is unknown; include units or map it in unit_map", name)
		}
		return id, err
	}
	locID := func(godown string) (int16, error) {
		if id, ok := p.LocationMap[godown]; ok {
			return int16(id), nil
		}
		if godown != "" {
			id, err := q.LocationByName(ctx, godown)
			if err == nil {
				return id, nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return 0, err
			}
			return 0, fmt.Errorf("godown %q is unknown; map it in location_map", godown)
		}
		l, err := q.DefaultBillingLocation(ctx)
		return l.ID, err
	}
	if contains(scope, "items") || contains(scope, "opening_stock") {
		for _, it := range d.Items {
			itemID, err := q.ItemIDByName(ctx, it.Name)
			if errors.Is(err, pgx.ErrNoRows) {
				if !contains(scope, "items") {
					continue
				}
				c, err := catID(it.Group)
				if err != nil {
					return nil, err
				}
				u, err := unitID(it.Unit)
				if err != nil {
					return nil, err
				}
				gst := decimalTrim(it.GST)
				if !httpx.GSTRates[gst] {
					gst = "18"
				}
				itemID, err = q.InsertImportedItem(ctx, dbq.InsertImportedItemParams{CategoryID: c, Name: it.Name, HsnCode: it.HSN,
					GstRate: decimal.RequireFromString(gst), BaseUnitID: u, CreatedBy: user})
				if err != nil {
					return nil, fmt.Errorf("item %q: %w", it.Name, err)
				}
				if err := q.UpsertItemUnit(ctx, dbq.UpsertItemUnitParams{ItemID: itemID, UnitID: u, FactorToBase: decimal.NewFromInt(1)}); err != nil {
					return nil, err
				}
				res["items"]++
			} else if err != nil {
				return nil, err
			}
			if !contains(scope, "opening_stock") {
				continue
			}
			for _, op := range it.Opening {
				qty, err := decimal.NewFromString(op.Qty)
				if err != nil || !qty.IsPositive() {
					continue
				}
				loc, err := locID(op.Godown)
				if err != nil {
					return nil, fmt.Errorf("item %q: %w", it.Name, err)
				}
				var cost *decimal.Decimal
				if v, err := decimal.NewFromString(op.Rate); err == nil {
					cost = &v
				}
				if err := q.StockIn(ctx, dbq.StockInParams{ItemID: itemID, LocationID: loc, Qty: qty.Round(3), Cost: cost,
					Kind: dbq.MovementKindOpening, UserID: user}); err != nil {
					return nil, err
				}
				res["opening_stock"]++
			}
		}
	}
	for _, kind := range []string{"customers", "suppliers"} {
		if !contains(scope, kind) {
			continue
		}
		list, pk := d.Customers, dbq.PartyKindCustomer
		if kind == "suppliers" {
			list, pk = d.Suppliers, dbq.PartyKindSupplier
		}
		for _, pt := range list {
			if _, err := q.PartyIDByName(ctx, pt.Name); err == nil {
				continue
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return nil, err
			}
			opening := decimal.Zero
			if contains(scope, "opening_balances") && pt.Opening != "" {
				opening, _ = decimal.NewFromString(pt.Opening)
			}
			var gstin, state *string
			if reGSTIN.MatchString(pt.GSTIN) {
				g, s := pt.GSTIN, pt.GSTIN[:2]
				gstin, state = &g, &s
			} else if pt.State != "" {
				s := pt.State
				state = &s
			}
			if _, err := q.InsertImportedParty(ctx, dbq.InsertImportedPartyParams{Kind: pk, Name: pt.Name, Mobile: nonEmpty(pt.Mobile),
				Gstin: gstin, StateCode: state, BillingAddress: nonEmpty(pt.Address), OpeningBalance: opening,
				OpeningDate: &imp.CutoverDate}); err != nil {
				return nil, fmt.Errorf("%s %q: %w", kind, pt.Name, err)
			}
			res[kind]++
		}
	}
	summary, _ := json.Marshal(res)
	total := 0
	for _, n := range res {
		total += n
	}
	cut := imp.CutoverDate
	syncID, err := q.InsertTallySync(ctx, dbq.InsertTallySyncParams{Direction: "import", Scope: strings.Join(scope, ","),
		PeriodTo: &cut, RecordCount: ptr(int32(total)), Summary: summary, Status: "completed", CreatedBy: user})
	if err != nil {
		return nil, err
	}
	if err := q.SetTallyImportStatus(ctx, dbq.SetTallyImportStatusParams{ID: p.ImportID, Status: "committed"}); err != nil {
		return nil, err
	}
	if err := audit.Log(ctx, q, "import", "tally", audit.ID(syncID), nil, res); err != nil {
		return nil, err
	}
	return res, nil
}

func nonEmpty(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	t := strings.TrimSpace(s)
	return &t
}
