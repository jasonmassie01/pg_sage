package value

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// /value reads only credited actions (perf v1.8.3, perf-selfexcl; perf
// gate offender on sage.action_log). It used to return every successful,
// credited action row to Go and its generic plan scanned all of
// action_log (`$1 IS NULL OR executed_at >= $1` is not an index range).
// Now it aggregates in SQL over the credited-actions index: rows read are
// the credited actions in the window, never the rest of the ledger.

// seedLedger adds n uncredited actions (failures, rollbacks and successes
// without credit) and the credited ones, then analyzes the table.
func seedLedger(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n int,
	credited map[time.Time]float64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO sage.action_log (executed_at, action_type,
		sql_executed, outcome)
		SELECT now() - g * interval '3 minutes', 'create_index', 'SELECT 1',
		       (ARRAY['success','failed','rolled_back'])[1 + g % 3]
		FROM generate_series(1, $1) g`, n); err != nil {
		t.Fatalf("seed uncredited actions: %v", err)
	}
	for at, minutes := range credited {
		feature := "create_index_concurrently"
		if minutes < 10 {
			feature = "analyze_table"
		}
		if _, err := pool.Exec(ctx, `INSERT INTO sage.action_log (executed_at, action_type,
			sql_executed, outcome, toil_minutes_saved, toil_model_version)
			VALUES ($1, $2, 'SELECT 1', 'success', $3, 1)`, at, feature, minutes); err != nil {
			t.Fatalf("seed credited action: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, "VACUUM (ANALYZE) sage.action_log"); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

func TestRealizedValueReadsCreditedActionsOnly(t *testing.T) {
	ctx := context.Background()
	src := newLedgerSource(t, "bounded")
	now := time.Now().UTC()
	old := now.AddDate(0, -2, 0)
	credited := map[time.Time]float64{now.Add(-time.Minute): 45,
		now.Add(-2 * time.Minute): 5, old: 30}
	seedLedger(t, ctx, src.Pool, 20000, credited)
	tx, err := src.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	before, err := testdb.XactScansOf(ctx, tx, "sage.action_log")
	if err != nil {
		t.Fatal(err)
	}
	var got Snapshot
	got.ByFeatureMinutes = map[string]float64{}
	if err := readRealized(ctx, tx, "bounded", Filter{}, &got); err != nil {
		t.Fatalf("readRealized: %v", err)
	}
	after, err := testdb.XactScansOf(ctx, tx, "sage.action_log")
	if err != nil {
		t.Fatal(err)
	}
	scans := after.Minus(before)
	if scans.Seq != 0 || scans.IndexFetch > int64(len(credited)) {
		t.Fatalf("realized value read %+v of sage.action_log, want no seq scan and at "+
			"most %d rows fetched (the credited actions)", scans, len(credited))
	}
	assertRealized(t, got, now, old)
}

func assertRealized(t *testing.T, got Snapshot, now, old time.Time) {
	t.Helper()
	month, week := 50.0, 50.0
	if now.Add(-2*time.Minute).Month() != now.Month() {
		month = 45
	}
	if now.Add(-2 * time.Minute).Before(startOfWeek(now)) {
		week = 45
	}
	if math.Abs(got.AllTimeMinutes-80) > 1e-9 || math.Abs(got.MonthMinutes-month) > 1e-9 ||
		math.Abs(got.WeekMinutes-week) > 1e-9 {
		t.Fatalf("all/month/week = %v/%v/%v, want 80/%v/%v", got.AllTimeMinutes,
			got.MonthMinutes, got.WeekMinutes, month, week)
	}
	if got.ByFeatureMinutes["create_index_concurrently"] != 75 ||
		got.ByFeatureMinutes["analyze_table"] != 5 {
		t.Fatalf("by feature = %v", got.ByFeatureMinutes)
	}
	if len(got.ByDatabaseMinutes) != 1 || got.ByDatabaseMinutes[0].Minutes != 80 ||
		got.ByDatabaseMinutes[0].Name != "bounded" {
		t.Fatalf("by database = %+v", got.ByDatabaseMinutes)
	}
	var trend float64
	for i, d := range got.TrendMinutes {
		trend += d.Minutes
		if i > 0 && got.TrendMinutes[i-1].Day >= d.Day {
			t.Fatalf("trend not ascending by day: %+v", got.TrendMinutes)
		}
	}
	oldDay := old.Format("2006-01-02")
	if trend != 80 || !strings.HasPrefix(got.TrendMinutes[0].Day, oldDay[:7]) {
		t.Fatalf("trend = %+v, want 80 minutes starting in %s", got.TrendMinutes, oldDay)
	}
}

// The window bounds are an index range of the credited-actions index in
// the generic plan too (the plan a prepared statement falls back to).
func TestRealizedValueGenericPlanIsAnIndexRange(t *testing.T) {
	ctx := context.Background()
	src := newLedgerSource(t, "generic")
	testdb.RequireServerVersion(t, src.Pool, 160000, "EXPLAIN (GENERIC_PLAN)")
	seedLedger(t, ctx, src.Pool, 20000, map[time.Time]float64{time.Now(): 45})
	conn, err := src.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	res, err := conn.Conn().PgConn().Exec(ctx, "EXPLAIN (GENERIC_PLAN) "+realizedSQL).
		ReadAll()
	if err != nil || len(res) != 1 {
		t.Fatalf("explain: %v", err)
	}
	var plan []string
	for _, row := range res[0].Rows {
		plan = append(plan, string(row[0]))
	}
	text := strings.Join(plan, "\n")
	if strings.Contains(text, "Seq Scan") ||
		!strings.Contains(text, "idx_action_log_value_credit") {
		t.Fatalf("generic plan of the realized value read:\n%s", text)
	}
}

// Since/Until still bound the credit (an index range, not a filter).
func TestRealizedValueHonorsTheWindow(t *testing.T) {
	ctx := context.Background()
	src := newLedgerSource(t, "window")
	now := time.Now().UTC()
	seedLedger(t, ctx, src.Pool, 100, map[time.Time]float64{
		now.Add(-time.Hour): 45, now.Add(-48 * time.Hour): 30})
	var got Snapshot
	got.ByFeatureMinutes = map[string]float64{}
	err := readRealized(ctx, src.Pool, "window", Filter{Since: now.Add(-2 * time.Hour),
		Until: now}, &got)
	if err != nil || got.AllTimeMinutes != 45 || len(got.TrendMinutes) != 1 {
		t.Fatalf("windowed realized value = %+v (%v), want 45 minutes on one day", got, err)
	}
}
