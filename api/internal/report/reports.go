package report

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/shopspring/decimal"
	"github.com/xuri/excelize/v2"

	"billing-api/internal/api"
	"billing-api/internal/app"
	"billing-api/internal/auth"
	"billing-api/internal/db/dbq"
	"billing-api/internal/docs"
	"billing-api/internal/httpx"
	"billing-api/internal/jobs"
)

// syncLimit: exports with more rows than this run as a job (202).
const syncLimit = 5000

func ReportRoutes(a *app.App) func(chi.Router) {
	h := handler{a}
	return func(r chi.Router) {
		r.Get("/{key}", httpx.H(h.run))
	}
}

// Params are a report run's filters.
type Params struct {
	Key      string    `json:"key"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	Location *int16    `json:"location,omitempty"`
	Category *int32    `json:"category,omitempty"`
	Party    *int64    `json:"party,omitempty"`
	Format   string    `json:"format"`
}

// Rows runs a report and returns typed rows (Money/Qty/Date values) and totals.
func Rows(ctx context.Context, a *app.App, p Params) (def, []map[string]any, map[string]any, error) {
	d := defs[p.Key]
	rows, err := a.Pool.Query(ctx, d.SQL, pgx.NamedArgs{"from": p.From, "to": p.To, "location": p.Location,
		"category": p.Category, "party": p.Party})
	if err != nil {
		return d, nil, nil, err
	}
	raw, err := pgx.CollectRows(rows, pgx.RowToMap)
	if err != nil {
		return d, nil, nil, err
	}
	sums := map[string]decimal.Decimal{}
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		row := map[string]any{}
		for _, c := range d.Columns {
			row[c.Key] = typed(r[c.Key], c.Type)
			if slices.Contains(d.Sum, c.Key) {
				if v, ok := asDecimal(r[c.Key]); ok {
					sums[c.Key] = sums[c.Key].Add(v)
				}
			}
		}
		out[i] = row
	}
	totals := map[string]any{}
	if len(d.Sum) > 0 {
		totals[d.Columns[0].Key] = "Total"
		for _, c := range d.Columns {
			if slices.Contains(d.Sum, c.Key) {
				totals[c.Key] = typed(sums[c.Key], c.Type)
			}
		}
	}
	return d, out, totals, nil
}

func asDecimal(v any) (decimal.Decimal, bool) {
	switch x := v.(type) {
	case decimal.Decimal:
		return x, true
	case int64:
		return decimal.NewFromInt(x), true
	case int32:
		return decimal.NewFromInt(int64(x)), true
	case nil:
		return decimal.Zero, true
	}
	return decimal.Zero, false
}

// typed converts a database value to its JSON rendering by column type.
func typed(v any, typ string) any {
	switch typ {
	case "money":
		d, _ := asDecimal(v)
		return api.M(d)
	case "qty":
		d, _ := asDecimal(v)
		return api.Q(d)
	case "percent":
		d, _ := asDecimal(v)
		return d.Round(2).StringFixed(2)
	case "date":
		if t, ok := v.(time.Time); ok {
			return api.Date(t)
		}
		return nil
	}
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func (h handler) run(w http.ResponseWriter, r *http.Request) error {
	key := chi.URLParam(r, "key")
	if _, ok := defs[key]; !ok {
		return httpx.NotFound("report " + key)
	}
	from, to, err := h.period(r)
	if err != nil {
		return err
	}
	format, err := httpx.QEnum(r, "format", "json", "xlsx", "pdf")
	if err != nil {
		return err
	}
	p := Params{Key: key, From: from, To: to, Format: "json"}
	if format != nil {
		p.Format = *format
	}
	if v, err := httpx.QInt64(r, "location_id"); err != nil {
		return err
	} else if v != nil {
		l := int16(*v)
		p.Location = &l
	}
	if v, err := httpx.QInt64(r, "category_id"); err != nil {
		return err
	} else if v != nil {
		c := int32(*v)
		p.Category = &c
	}
	if p.Party, err = httpx.QInt64(r, "party_id"); err != nil {
		return err
	}
	page, err := httpx.ParsePage(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	d, rows, totals, err := Rows(ctx, h.a, p)
	if err != nil {
		return err
	}
	if p.Format == "json" {
		out := api.Report{Columns: d.Columns, Totals: totals}
		start := min(int(page.Offset()), len(rows))
		end := min(start+page.Size, len(rows))
		out.Rows = rows[start:end]
		out.Meta.Page, out.Meta.PageSize, out.Meta.Total = page.Page, page.Size, len(rows)
		httpx.JSON(w, http.StatusOK, out)
		return nil
	}
	if len(rows) > syncLimit {
		var job dbq.Job
		err := h.a.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
			var err error
			if job, err = jobs.Create(ctx, q, "report_export", p); err != nil {
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
	_, link, err := export(ctx, h.a, p, d, rows, totals)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, link)
	return nil
}

// export renders the file, uploads it and returns its path and a signed link.
func export(ctx context.Context, a *app.App, p Params, d def, rows []map[string]any, totals map[string]any) (string, api.SignedURL, error) {
	s, _ := a.Q.GetSettings(ctx)
	subtitle := fmt.Sprintf("From %s to %s", docs.DMY(p.From), docs.DMY(p.To))
	name := fmt.Sprintf("reports/%s-%s-%s-%s", p.Key, p.From.Format("20060102"), p.To.Format("20060102"), uuid.NewString()[:8])
	if p.Format == "pdf" {
		pdf, err := docs.Table(s, d.Title, subtitle, d.Columns, rows, totals)
		if err != nil {
			return "", api.SignedURL{}, err
		}
		link, err := docs.Publish(ctx, a, name+".pdf", pdf)
		return name + ".pdf", link, err
	}
	xlsx, err := Excel(d.Title, subtitle, d.Columns, rows, totals)
	if err != nil {
		return "", api.SignedURL{}, err
	}
	link, err := docs.PublishFile(ctx, a, name+".xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", xlsx)
	return name + ".xlsx", link, err
}

// Excel writes a report as a workbook (numbers as numbers, Indian format).
func Excel(title, subtitle string, cols []api.ReportColumn, rows []map[string]any, totals map[string]any) ([]byte, error) {
	f := excelize.NewFile()
	defer f.Close()
	sheet := "Report"
	_ = f.SetSheetName("Sheet1", sheet)
	_ = f.SetCellValue(sheet, "A1", title)
	_ = f.SetCellValue(sheet, "A2", subtitle)
	bold, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	money, _ := f.NewStyle(&excelize.Style{CustomNumFmt: ptr("#,##,##0.00")})
	qty, _ := f.NewStyle(&excelize.Style{CustomNumFmt: ptr("0.000")})
	date, _ := f.NewStyle(&excelize.Style{CustomNumFmt: ptr("dd-mm-yyyy")})
	_ = f.SetCellStyle(sheet, "A1", "A1", bold)
	header := make([]any, len(cols))
	for i, c := range cols {
		header[i] = c.Label
	}
	_ = f.SetSheetRow(sheet, "A4", &header)
	last, _ := excelize.CoordinatesToCellName(len(cols), 4)
	_ = f.SetCellStyle(sheet, "A4", last, bold)
	write := func(rowNo int, r map[string]any) {
		for i, c := range cols {
			cell, _ := excelize.CoordinatesToCellName(i+1, rowNo)
			switch v := r[c.Key].(type) {
			case api.Money:
				_ = f.SetCellValue(sheet, cell, v.D().InexactFloat64())
				_ = f.SetCellStyle(sheet, cell, cell, money)
			case api.Qty:
				_ = f.SetCellValue(sheet, cell, v.D().InexactFloat64())
				_ = f.SetCellStyle(sheet, cell, cell, qty)
			case api.Date:
				_ = f.SetCellValue(sheet, cell, v.T())
				_ = f.SetCellStyle(sheet, cell, cell, date)
			default:
				_ = f.SetCellValue(sheet, cell, v)
			}
		}
	}
	for i, r := range rows {
		write(5+i, r)
	}
	if len(totals) > 0 {
		write(5+len(rows), totals)
		a, _ := excelize.CoordinatesToCellName(1, 5+len(rows))
		b, _ := excelize.CoordinatesToCellName(len(cols), 5+len(rows))
		_ = f.SetCellStyle(sheet, a, b, bold)
	}
	_ = f.SetColWidth(sheet, "A", "Z", 16)
	buf, err := f.WriteToBuffer()
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func ptr[T any](v T) *T { return &v }

// ExportArgs runs a large report export as a job.
type ExportArgs struct {
	JobID uuid.UUID `json:"job_id"`
}

func (ExportArgs) Kind() string { return "report_export" }

type exportWorker struct {
	river.WorkerDefaults[ExportArgs]
	a *app.App
}

func RegisterWorkers(w *river.Workers, a *app.App) {
	river.AddWorker(w, &exportWorker{a: a})
}

func (wk *exportWorker) Work(ctx context.Context, job *river.Job[ExportArgs]) error {
	return jobs.Track(ctx, wk.a, job.Args.JobID, func(ctx context.Context) (jobs.Outcome, error) {
		j, err := wk.a.Q.GetJob(ctx, job.Args.JobID)
		if err != nil {
			return jobs.Outcome{}, err
		}
		var p Params
		if err := json.Unmarshal(j.Params, &p); err != nil {
			return jobs.Outcome{}, err
		}
		ctx = auth.WithUser(ctx, auth.User{})
		d, rows, totals, err := Rows(ctx, wk.a, p)
		if err != nil {
			return jobs.Outcome{}, err
		}
		path, _, err := export(ctx, wk.a, p, d, rows, totals)
		if err != nil {
			return jobs.Outcome{}, err
		}
		return jobs.Outcome{Result: map[string]int{"rows": len(rows)}, Path: path}, nil
	})
}
