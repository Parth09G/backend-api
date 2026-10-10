package inventory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/xuri/excelize/v2"

	"billing-api/internal/api"
	"billing-api/internal/app"
	"billing-api/internal/audit"
	"billing-api/internal/auth"
	"billing-api/internal/db/dbq"
	"billing-api/internal/httpx"
	"billing-api/internal/jobs"
)

const xlsxType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"

// column of the per-category Excel template (FR-IN-06).
type column struct {
	Label string
	Attr  *dbq.CategoryAttribute
	Key   string // name | hsn | gst | base_unit | reorder | sku | opening | opening_cost
	LocID int16
}

func templateColumns(ctx context.Context, q *dbq.Queries, catID int32) ([]column, dbq.GetCategoryRow, error) {
	cat, err := q.GetCategory(ctx, catID)
	if err != nil {
		return nil, cat, notFound(err, "category")
	}
	attrs, err := q.CategoryAttributes(ctx, []int32{catID})
	if err != nil {
		return nil, cat, err
	}
	locs, err := q.ActiveLocations(ctx)
	if err != nil {
		return nil, cat, err
	}
	cols := []column{{Label: "Name", Key: "name"}}
	for i := range attrs {
		a := attrs[i]
		label := a.Label
		if a.ValueUnit != nil && *a.ValueUnit != "" {
			label += " (" + *a.ValueUnit + ")"
		}
		if a.IsRequired {
			label += " *"
		}
		cols = append(cols, column{Label: label, Attr: &a})
	}
	cols = append(cols,
		column{Label: "HSN code *", Key: "hsn"},
		column{Label: "GST rate % *", Key: "gst"},
		column{Label: "Base unit *", Key: "base_unit"},
		column{Label: "Reorder level", Key: "reorder"},
		column{Label: "SKU", Key: "sku"},
	)
	for _, l := range locs {
		cols = append(cols, column{Label: "Opening qty - " + l.Name, Key: "opening", LocID: l.ID})
	}
	cols = append(cols, column{Label: "Opening cost per base unit", Key: "opening_cost"})
	return cols, cat, nil
}

func (h handler) importTemplate(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	cols, cat, err := templateColumns(ctx, h.a.Q, int32(id))
	if err != nil {
		return err
	}
	f := excelize.NewFile()
	defer f.Close()
	sheet := "Items"
	_ = f.SetSheetName("Sheet1", sheet)
	header := make([]any, len(cols))
	for i, c := range cols {
		header[i] = c.Label
	}
	if err := f.SetSheetRow(sheet, "A1", &header); err != nil {
		return err
	}
	style, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}, Fill: excelize.Fill{Type: "pattern", Color: []string{"#E2E8F0"}, Pattern: 1}})
	last, _ := excelize.CoordinatesToCellName(len(cols), 1)
	_ = f.SetCellStyle(sheet, "A1", last, style)
	_ = f.SetColWidth(sheet, "A", "Z", 18)

	// Instructions: allowed units, GST rates and select options.
	info := "Instructions"
	_, _ = f.NewSheet(info)
	units, _ := h.a.Q.CategoryUnits(ctx, []int32{cat.ID})
	rows := [][]any{
		{"Category", cat.Name},
		{"Columns marked * are required. Leave Name empty to build it from the attributes."},
		{"GST rates", "0, 0.25, 3, 5, 12, 18, 28, 40"},
	}
	var syms []string
	for _, u := range units {
		syms = append(syms, u.Symbol)
	}
	if len(syms) > 0 {
		rows = append(rows, []any{"Units (symbol)", strings.Join(syms, ", ")})
	}
	for _, c := range cols {
		if c.Attr != nil && len(c.Attr.Options) > 0 {
			rows = append(rows, []any{c.Attr.Label + " options", strings.Join(c.Attr.Options, ", ")})
		}
	}
	for i, row := range rows {
		cell, _ := excelize.CoordinatesToCellName(1, i+1)
		_ = f.SetSheetRow(info, cell, &row)
	}
	_ = f.SetColWidth(info, "A", "B", 30)
	buf, err := f.WriteToBuffer()
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", xlsxType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="items-%s.xlsx"`, slug(cat.Name)))
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(buf.Bytes())
	return err
}

func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, s)
}

// ImportArgs runs an item import job.
type ImportArgs struct {
	JobID uuid.UUID `json:"job_id"`
}

func (ImportArgs) Kind() string { return "item_import" }

type importParams struct {
	CategoryID int64     `json:"category_id"`
	DryRun     bool      `json:"dry_run"`
	Path       string    `json:"path"`
	UserID     uuid.UUID `json:"user_id"`
}

// RowError is one problem in the uploaded sheet.
type RowError struct {
	Row     int    `json:"row"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

type importResult struct {
	Rows    int        `json:"rows"`
	Created int        `json:"created"`
	DryRun  bool       `json:"dry_run"`
	Errors  []RowError `json:"errors"`
}

// importItems accepts the file and queues the import (202 + job).
func (h handler) importItems(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		return httpx.Validation("file", "upload an .xlsx file (max 10 MB) as multipart field \"file\"")
	}
	catID, err := strconv.ParseInt(r.FormValue("category_id"), 10, 64)
	if err != nil || catID < 1 {
		return httpx.Validation("category_id", "is required")
	}
	if _, err := h.a.Q.GetCategory(ctx, int32(catID)); err != nil {
		return httpx.Validation("category_id", "unknown category")
	}
	dryRun, _ := strconv.ParseBool(r.FormValue("dry_run"))
	f, _, err := r.FormFile("file")
	if err != nil {
		return httpx.Validation("file", "is required")
	}
	defer f.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(f); err != nil {
		return err
	}
	if _, err := excelize.OpenReader(bytes.NewReader(buf.Bytes())); err != nil {
		return httpx.Validation("file", "is not a valid .xlsx file")
	}
	user := auth.MustUser(r)
	var job dbq.Job
	err = h.a.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		var err error
		path := "imports/items/" + uuid.NewString() + ".xlsx"
		if err := h.a.Storage.Upload(ctx, path, xlsxType, buf.Bytes()); err != nil {
			return err
		}
		job, err = jobs.Create(ctx, q, "item_import", importParams{CategoryID: catID, DryRun: dryRun, Path: path, UserID: user.ID})
		if err != nil {
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

// RegisterWorkers adds the inventory job workers.
func RegisterWorkers(w *river.Workers, a *app.App) {
	river.AddWorker(w, &importWorker{a: a})
}

type importWorker struct {
	river.WorkerDefaults[ImportArgs]
	a *app.App
}

var errRollback = errors.New("rollback")

func (wk *importWorker) Work(ctx context.Context, job *river.Job[ImportArgs]) error {
	a := wk.a
	return jobs.Track(ctx, a, job.Args.JobID, func(ctx context.Context) (jobs.Outcome, error) {
		j, err := a.Q.GetJob(ctx, job.Args.JobID)
		if err != nil {
			return jobs.Outcome{}, err
		}
		var p importParams
		if err := json.Unmarshal(j.Params, &p); err != nil {
			return jobs.Outcome{}, err
		}
		if u, err := a.Q.GetAppUser(ctx, p.UserID); err == nil {
			ctx = auth.WithUser(ctx, auth.User{ID: u.ID, Email: u.Email, FullName: u.FullName, Role: string(u.Role), Row: u})
		}
		data, err := a.Storage.Download(ctx, p.Path)
		if err != nil {
			return jobs.Outcome{}, fmt.Errorf("read uploaded file: %w", err)
		}
		res, err := importRows(ctx, a, p, data)
		if err != nil {
			return jobs.Outcome{}, err
		}
		return jobs.Outcome{Result: res}, nil
	})
}

// importRows validates every row and, unless dry run or any row is
// invalid, creates all items in one transaction (all-or-nothing).
func importRows(ctx context.Context, a *app.App, p importParams, data []byte) (importResult, error) {
	res := importResult{DryRun: p.DryRun, Errors: []RowError{}}
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return res, fmt.Errorf("not a valid .xlsx file: %w", err)
	}
	defer f.Close()
	sheet := f.GetSheetName(0)
	rows, err := f.GetRows(sheet)
	if err != nil {
		return res, err
	}
	if len(rows) < 2 {
		return res, errors.New("the sheet has no item rows")
	}
	h := handler{a}
	err = a.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		cols, _, err := templateColumns(ctx, q, int32(p.CategoryID))
		if err != nil {
			return err
		}
		index := map[string]int{}
		for i, hdr := range rows[0] {
			index[strings.TrimSpace(hdr)] = i
		}
		for _, c := range cols {
			if _, ok := index[c.Label]; !ok && (c.Key == "hsn" || c.Key == "gst" || c.Key == "base_unit") {
				return fmt.Errorf("column %q is missing; download a fresh template", c.Label)
			}
		}
		units, err := q.ListUnits(ctx)
		if err != nil {
			return err
		}
		unitBySym := map[string]int64{}
		for _, u := range units {
			unitBySym[strings.ToLower(u.Symbol)] = int64(u.ID)
			unitBySym[strings.ToLower(u.Name)] = int64(u.ID)
		}
		for n, row := range rows[1:] {
			rowNo := n + 2
			get := func(label string) string {
				if i, ok := index[label]; ok && i < len(row) {
					return strings.TrimSpace(row[i])
				}
				return ""
			}
			if strings.Join(row, "") == "" {
				continue
			}
			res.Rows++
			in := api.ItemInput{CategoryID: p.CategoryID, Attributes: map[string]any{}}
			var cost string
			for _, c := range cols {
				v := get(c.Label)
				switch {
				case c.Attr != nil:
					if v != "" {
						in.Attributes[c.Attr.Code] = v
					}
				case c.Key == "name" && v != "":
					in.Name = &v
				case c.Key == "hsn":
					in.HSNCode = v
				case c.Key == "gst":
					in.GSTRate = strings.TrimSuffix(v, "%")
				case c.Key == "base_unit":
					in.BaseUnitID = unitBySym[strings.ToLower(v)]
					if in.BaseUnitID == 0 {
						res.Errors = append(res.Errors, RowError{rowNo, c.Label, "unknown unit " + strconv.Quote(v)})
					}
				case c.Key == "reorder" && v != "":
					in.ReorderLevel = &v
				case c.Key == "sku" && v != "":
					in.SKU = &v
				case c.Key == "opening" && v != "" && v != "0":
					in.OpeningStock = append(in.OpeningStock, api.OpeningStockInput{LocationID: int64(c.LocID), Qty: v})
				case c.Key == "opening_cost":
					cost = v
				}
			}
			for i := range in.OpeningStock {
				if cost != "" {
					in.OpeningStock[i].UnitCost = &cost
				}
			}
			if in.BaseUnitID == 0 {
				continue
			}
			if err := httpx.Validate(&in); err != nil {
				res.Errors = append(res.Errors, rowErrors(rowNo, err)...)
				continue
			}
			// Savepoint per row: a database error on one row must not abort the rest.
			sp, err := tx.Begin(ctx)
			if err != nil {
				return err
			}
			if _, err := h.insertItem(ctx, q.WithTx(sp), in); err != nil {
				_ = sp.Rollback(ctx)
				res.Errors = append(res.Errors, rowErrors(rowNo, httpx.ToProblem(ctx, err))...)
				continue
			}
			if err := sp.Commit(ctx); err != nil {
				return err
			}
			res.Created++
		}
		if len(res.Errors) > 0 || p.DryRun {
			return errRollback
		}
		return audit.Log(ctx, q, "import", "item", nil, nil, map[string]any{"category_id": p.CategoryID, "created": res.Created})
	})
	if errors.Is(err, errRollback) {
		if len(res.Errors) > 0 || p.DryRun {
			res.Created = 0
		}
		return res, nil
	}
	return res, err
}

func rowErrors(row int, err error) []RowError {
	var pr *httpx.Problem
	if !errors.As(err, &pr) {
		return []RowError{{Row: row, Message: err.Error()}}
	}
	if len(pr.Errors) == 0 {
		return []RowError{{Row: row, Message: pr.Detail}}
	}
	out := make([]RowError, len(pr.Errors))
	for i, fe := range pr.Errors {
		out[i] = RowError{Row: row, Field: fe.Field, Message: fe.Message}
	}
	return out
}
