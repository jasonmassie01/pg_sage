package analyzer

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Phase 0 item 8: the regression baseline used to read every 'queries'
// snapshot of the lookback (7 days x 1,440 a day x 500 statements) on
// every analyzer cycle and thin them to 100 in Go. At most
// maxHistorySamples snapshots are sampled, however many exist. v1.8.3
// (performance gate offender 4) samples one snapshot per epoch-aligned
// time bucket instead of every n-th by rank, so the sample is stable from
// cycle to cycle and each snapshot is decoded once (history_incremental_
// db_test.go); with no more snapshots than the cap every one is still
// used, the estimator the analyzer has always used for short histories.

// No concurrent access tests: buildHistoricalAverages is called only by
// the single analyzer cycle goroutine and holds no shared state.

func cleanQuerySnapshots(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		"DELETE FROM sage.snapshots WHERE category = 'queries'"); err != nil {
		t.Fatalf("clean query snapshots: %v", err)
	}
}

func historyDayStart(d int) time.Time {
	return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -d)
}

func insertQuerySnapshot(t *testing.T, pool *pgxpool.Pool, at time.Time, data any) {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO sage.snapshots
		(collected_at, category, data) VALUES ($1, 'queries', $2::jsonb)`,
		at, string(raw)); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}
}

// seedHistory writes perDay snapshots for each of days days; queryid 7's
// mean_exec_time is the snapshot's index within its day (0..perDay-1).
func seedHistory(t *testing.T, pool *pgxpool.Pool, days, perDay int) {
	t.Helper()
	cleanQuerySnapshots(t, pool)
	t.Cleanup(func() { cleanQuerySnapshots(t, pool) })
	for d := days - 1; d >= 0; d-- {
		for i := 0; i < perDay; i++ {
			step := 23 * time.Hour / time.Duration(perDay)
			at := historyDayStart(d).Add(time.Minute + time.Duration(i)*step)
			insertQuerySnapshot(t, pool, at, []map[string]any{
				{"queryid": 7, "mean_exec_time": float64(i), "query": "SELECT 1"},
				{"queryid": 8, "mean_exec_time": 50.0},
			})
		}
	}
}

// With fewer snapshots than the cap, every one is used (the estimator the
// analyzer has always used).
func TestHistoricalAverages_SmallHistoryUsesAll(t *testing.T) {
	pool := phase2Pool(t)
	seedHistory(t, pool, 2, 4)
	a, rec := recordingAnalyzer(t, 3)
	if got := a.buildHistoricalAverages(context.Background())[7]; got != 1.5 {
		t.Fatalf("avg(7) = %v, want 1.5", got)
	}
	if n := decodedSnapshots(t, rec); n != 8 {
		t.Fatalf("expanded %d snapshots, want all 8", n)
	}
}

// A zero or negative lookback uses the default (7 days) instead of
// reading nothing; snapshots outside the lookback are ignored.
func TestHistoricalAverages_LookbackDefaultsAndWindow(t *testing.T) {
	pool := phase2Pool(t)
	cleanQuerySnapshots(t, pool)
	t.Cleanup(func() { cleanQuerySnapshots(t, pool) })
	insertQuerySnapshot(t, pool, time.Now().Add(-3*24*time.Hour),
		[]map[string]any{{"queryid": 9, "mean_exec_time": 12.0}})
	insertQuerySnapshot(t, pool, time.Now().Add(-30*24*time.Hour),
		[]map[string]any{{"queryid": 9, "mean_exec_time": 1000.0}})
	for _, lookback := range []int{0, -4} {
		cfg := phase2Config()
		cfg.Analyzer.RegressionLookbackDays = lookback
		a := New(pool, cfg, nil, nil, nil, nil, nil, noopLog)
		if got := a.buildHistoricalAverages(context.Background())[9]; got != 12 {
			t.Fatalf("lookback %d: avg = %v, want 12 (default 7-day window)", lookback, got)
		}
	}
}

// Malformed snapshots (null, an object, non-numeric fields) are skipped,
// not fatal to the whole baseline.
func TestHistoricalAverages_MalformedSnapshotsSkipped(t *testing.T) {
	pool := phase2Pool(t)
	cleanQuerySnapshots(t, pool)
	t.Cleanup(func() { cleanQuerySnapshots(t, pool) })
	day := historyDayStart(1)
	insertQuerySnapshot(t, pool, day.Add(time.Hour), []map[string]any{
		{"queryid": "x", "mean_exec_time": 1.0}, {"queryid": 5, "mean_exec_time": "slow"},
		{"queryid": 6, "mean_exec_time": 4.0}})
	insertQuerySnapshot(t, pool, day.Add(2*time.Hour), nil)
	insertQuerySnapshot(t, pool, day.Add(3*time.Hour), map[string]any{"queryid": 1})
	insertQuerySnapshot(t, pool, day.Add(4*time.Hour), []map[string]any{
		{"queryid": 6, "mean_exec_time": 8.0}})
	// The only snapshot of the previous day is an object: sampled, skipped.
	insertQuerySnapshot(t, pool, historyDayStart(2).Add(time.Hour),
		map[string]any{"queryid": 6, "mean_exec_time": 1000.0})
	a := New(pool, phase2Config(), nil, nil, nil, nil, nil, noopLog)
	a.eval = newCycleEval()
	avgs := a.buildHistoricalAverages(context.Background())
	if avgs == nil || avgs[6] != 6 || len(avgs) != 1 {
		t.Fatalf("averages = %v, want only queryid 6 = 6", avgs)
	}
	if a.eval.failed["query_regression"] {
		t.Fatal("malformed rows failed the regression rule")
	}
}

// A query error marks the regression rule failed (fail closed: its
// findings are not resolved this cycle) and returns no baseline.
func TestHistoricalAverages_ErrorFailsRule(t *testing.T) {
	pool := phase2Pool(t)
	a := New(pool, phase2Config(), nil, nil, nil, nil, nil, noopLog)
	a.eval = newCycleEval()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if avgs := a.buildHistoricalAverages(ctx); avgs != nil {
		t.Fatalf("canceled baseline = %v, want nil", avgs)
	}
	if !a.eval.failed["query_regression"] {
		t.Fatal("query error did not fail query_regression")
	}
}
