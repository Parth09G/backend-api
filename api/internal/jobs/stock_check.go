package jobs

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/riverqueue/river"

	"billing-api/internal/app"
	"billing-api/internal/db/dbq"
)

// StockCheckArgs: nightly check that every stock_balance row equals the sum
// of its stock_movement rows; mismatches notify the Super Admin (HLD 6.8).
type StockCheckArgs struct{}

func (StockCheckArgs) Kind() string { return "stock_check" }

type StockCheckWorker struct {
	river.WorkerDefaults[StockCheckArgs]
	a *app.App
}

func (w *StockCheckWorker) Work(ctx context.Context, _ *river.Job[StockCheckArgs]) error {
	q := w.a.Q
	rows, err := q.StockMismatches(ctx)
	if err != nil {
		return err
	}
	role := dbq.UserRoleSuperAdmin
	for _, m := range rows {
		body := fmt.Sprintf("Item %d at location %d: balance %s, movements %s", m.ItemID, m.LocationID,
			m.BalanceQty.StringFixed(3), m.MovementQty.StringFixed(3))
		item := m.ItemID
		if err := q.InsertNotification(ctx, dbq.InsertNotificationParams{ForRole: &role, Kind: "other",
			Title: "Stock mismatch found by nightly check", Body: &body, ItemID: &item}); err != nil {
			return err
		}
	}
	slog.InfoContext(ctx, "stock check done", "mismatches", len(rows))
	return nil
}
