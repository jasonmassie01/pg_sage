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
// every analyzer cycle and thin them in Go. It now samples in SQL: the
// first and last snapshot of each day, at most maxHistorySamples, so the
// jsonb expanded per cycle is bounded by days, not by snapshot count.

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
			at := historyDayStart(d).Add(time.Minute + time.Duration(i)*20*time.Minute)
			insertQuerySnapshot(t, pool, at, []map[string]any{
				{"queryid": 7, "mean_exec_time": float64(i), "query": "SELECT 1"},
				{"queryid": 8, "mean_exec_time": 50.0},
			})
		}
	}
}

func historyExpansionLoops(t *testing.T, pool *pgxpool.Pool, days int) int {
	t.Helper()
	var raw string
	if err := pool.QueryRow(context.Background(),
		"EXPLAIN (ANALYZE, FORMAT JSON) "+historicalAveragesSQL, days, maxHistorySamples).
		Scan(&raw); err != nil {
		t.Fatalf("explain: %v", err)
	}
	var plans []map[string]any
	if err := json.Unmarshal([]byte(raw), &plans); err != nil {
		t.Fatal(err)
	}
	var find func(n map[string]any) int
	find = func(n map[string]any) int {
		if n["Node Type"] == "Function Scan" {
			return int(n["Actual Loops"].(float64))
		}
		children, _ := n["Plans"].([]any)
		for _, c := range children {
			if v := find(c.(map[string]any)); v > 0 {
				return v
			}
		}
		return 0
	}
	return find(plans[0]["Plan"].(map[string]any))
}

func TestHistoricalAverages_FirstAndLastPerDay(t *testing.T) {
	pool := phase2Pool(t)
	const days, perDay = 3, 60
	seedHistory(t, pool, days, perDay)
	cfg := phase2Config()
	cfg.Analyzer.RegressionLookbackDays = days + 1
	a := New(pool, cfg, nil, nil, nil, nil, nil, noopLog)
	avgs := a.buildHistoricalAverages(context.Background())
	// Each day contributes its first (0) and last (perDay-1) reading.
	if got, want := avgs[7], float64(perDay-1)/2; got != want {
		t.Fatalf("avg(7) = %v, want %v", got, want)
	}
	if avgs[8] != 50 {
		t.Fatalf("avg(8) = %v, want 50", avgs[8])
	}
	if loops := historyExpansionLoops(t, pool, days+1); loops < 1 || loops > 2*days {
		t.Fatalf("expanded %d snapshots for %d days x %d, want at most %d", loops, days,
			perDay, 2*days)
	}
}

// The sample is capped at maxHistorySamples however long the lookback.
func TestHistoricalAverages_CappedSamples(t *testing.T) {
	pool := phase2Pool(t)
	days := maxHistorySamples/2 + 10
	seedHistory(t, pool, days, 3)
	if loops := historyExpansionLoops(t, pool, days+1); loops != maxHistorySamples {
		t.Fatalf("expanded %d snapshots, want the cap %d", loops, maxHistorySamples)
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
