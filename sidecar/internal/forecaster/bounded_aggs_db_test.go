package forecaster

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Dogfood lifeos-1 finding 4: the sequence aggregation expanded every
// element of every 'sequences' snapshot in the lookback (lifeos: 1,999
// snapshots x 12,000 sequences, ~489 MB of jsonb, > 70 s), starving the
// collector and the probes. Both daily aggregations now read at most the
// first and last non-empty snapshot of each day: the work is bounded by
// the lookback in days, not by how many snapshots were taken. For
// monotonic counters the daily totals are unchanged (they telescope).

const (
	boundedDays      = 3
	boundedPerDay    = 40
	boundedSequences = 500
)

// dayStart is the start of the UTC day d days ago.
func dayStart(d int) time.Time {
	return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -d)
}

// seedSequenceSnapshots writes boundedPerDay snapshots per day of
// boundedSequences sequences. public.hot_seq climbs through each day;
// public.reset_seq is restarted mid-day (its last value is below its day
// maximum); public.cold_seq stays under 1%.
func seedSequenceSnapshots(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	cleanupSnapshots(t, pool, ctx, "sequences")
	for d := boundedDays - 1; d >= 0; d-- {
		for i := 0; i < boundedPerDay; i++ {
			at := dayStart(d).Add(time.Duration(i) * 30 * time.Minute)
			hot := float64(10*(boundedDays-d)) + float64(i)/10
			reset := 50.0
			if i >= boundedPerDay/2 {
				reset = 5.0
			}
			elems := []map[string]any{
				{"schemaname": "public", "sequencename": "hot_seq", "pct_used": hot,
					"max_value": 2147483647},
				{"schemaname": "public", "sequencename": "reset_seq", "pct_used": reset,
					"max_value": 2147483647},
				{"schemaname": "public", "sequencename": "cold_seq", "pct_used": 0.2,
					"max_value": 2147483647}}
			for k := 0; k < boundedSequences; k++ {
				elems = append(elems, map[string]any{"schemaname": "t",
					"sequencename": fmt.Sprintf("s%d", k), "pct_used": 0.0, "max_value": 100})
			}
			insertSnapshot(t, ctx, pool, at, "sequences", elems)
		}
		insertSnapshot(t, ctx, pool, dayStart(d).Add(23*time.Hour), "sequences", []any{})
	}
}

func insertSnapshot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, at time.Time,
	category string, data any) {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.snapshots (collected_at, category, data)
		VALUES ($1, $2, $3::jsonb)`, at, category, string(raw)); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}
}

func TestSeqAggs_LastNonEmptySnapshotPerDay(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedSequenceSnapshots(t, ctx, pool)
	start := time.Now()
	got, err := QueryDailySeqAggs(ctx, pool, boundedDays+1)
	if err != nil {
		t.Fatalf("seq aggs: %v", err)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("seq aggs took %s", el)
	}
	byDay := map[string]map[string]float64{}
	for _, a := range got {
		k := a.Day.UTC().Format("2006-01-02")
		if byDay[k] == nil {
			byDay[k] = map[string]float64{}
		}
		byDay[k][a.SeqName] = a.PctUsed
	}
	if len(byDay) != boundedDays {
		t.Fatalf("days = %d (%v), want %d", len(byDay), byDay, boundedDays)
	}
	for d := 0; d < boundedDays; d++ {
		k := dayStart(d).Format("2006-01-02")
		wantHot := float64(10*(boundedDays-d)) + float64(boundedPerDay-1)/10
		if byDay[k]["public.hot_seq"] != wantHot || byDay[k]["public.reset_seq"] != 5 {
			t.Fatalf("day %s = %v, want hot %v and reset 5 (the last reading)", k,
				byDay[k], wantHot)
		}
		if _, ok := byDay[k]["public.cold_seq"]; ok {
			t.Fatalf("day %s lists a sequence under 1%%", k)
		}
	}
}

// Bounded work: the jsonb of at most two snapshots per day is expanded,
// however many were taken that day.
func TestSeqAndQueryAggs_ExpandAFewSnapshotsPerDay(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedSequenceSnapshots(t, ctx, pool)
	seedBoundedQuerySnapshots(t, ctx, pool)
	for name, sql := range map[string]string{"seq": seqAggsSQL, "query": queryAggsSQL} {
		loops := functionScanLoops(t, ctx, pool, sql)
		if loops < 1 || loops > 2*(boundedDays+1) {
			t.Fatalf("%s aggregation expanded %d snapshots, want at most %d", name, loops,
				2*(boundedDays+1))
		}
	}
}

// functionScanLoops runs sql under EXPLAIN ANALYZE and returns the loops
// of its jsonb expansion (the Function Scan node).
func functionScanLoops(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	sql string) int {
	t.Helper()
	var raw string
	if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+sql,
		boundedDays+1).Scan(&raw); err != nil {
		t.Fatalf("explain: %v", err)
	}
	var plans []map[string]any
	if err := json.Unmarshal([]byte(raw), &plans); err != nil {
		t.Fatalf("plan json: %v", err)
	}
	return findLoops(plans[0]["Plan"].(map[string]any))
}

func findLoops(node map[string]any) int {
	if node["Node Type"] == "Function Scan" {
		return int(node["Actual Loops"].(float64))
	}
	children, _ := node["Plans"].([]any)
	for _, c := range children {
		if n := findLoops(c.(map[string]any)); n > 0 {
			return n
		}
	}
	return 0
}

// seedBoundedQuerySnapshots writes boundedPerDay snapshots per day of one
// query whose calls grow by 3 per snapshot, plus empty snapshots at the
// start and end of each day.
func seedBoundedQuerySnapshots(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	cleanupSnapshots(t, pool, ctx, "queries")
	calls := 100
	for d := boundedDays - 1; d >= 0; d-- {
		insertSnapshot(t, ctx, pool, dayStart(d), "queries", nil)
		for i := 0; i < boundedPerDay; i++ {
			at := dayStart(d).Add(time.Minute + time.Duration(i)*30*time.Minute)
			calls += 3
			insertSnapshot(t, ctx, pool, at, "queries", []map[string]any{
				{"queryid": 42, "calls": calls}, {"queryid": 43, "calls": 7}})
		}
		insertSnapshot(t, ctx, pool, dayStart(d).Add(23*time.Hour), "queries", []any{})
	}
}

// The bounded query aggregation equals the exhaustive one for monotonic
// counters: day 1 counts from its first sample, later days from the
// previous day's last sample.
func TestQueryAggs_TotalsUnchangedForMonotonicCounters(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedBoundedQuerySnapshots(t, ctx, pool)
	got, err := QueryDailyQueryAggs(ctx, pool, boundedDays+1)
	if err != nil || len(got) != boundedDays {
		t.Fatalf("query aggs = %+v (%v)", got, err)
	}
	for i, a := range got {
		want := float64(3 * boundedPerDay)
		if i == 0 {
			want = float64(3 * (boundedPerDay - 1))
		}
		if a.TotalCalls != want {
			t.Fatalf("day %d (%s) total = %v, want %v", i, a.Day, a.TotalCalls, want)
		}
	}
}
