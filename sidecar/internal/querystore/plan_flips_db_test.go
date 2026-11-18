package querystore

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PlanFlips (lab-01b) reads, per query, the fingerprint at or before the
// step and the first different one after it. No concurrent access tests:
// one read-only statement with no shared state.

func insertHash(t *testing.T, ctx context.Context, pool *pgxpool.Pool, qid int64,
	at time.Time, hash *string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO sage.query_store
		(queryid, captured_at, calls, total_exec_time, mean_exec_time, rows, plan_hash)
		VALUES ($1, $2, 1, 1, 1, 1, $3)`, qid, at, hash); err != nil {
		t.Fatalf("insert sample: %v", err)
	}
}

func strPtr(s string) *string { return &s }

func TestPlanFlipsReadsTheFingerprintAroundTheStep(t *testing.T) {
	pool, ctx := requireDB(t)
	defer pool.Close()
	const flips, steady, late, after, none = int64(7_400_000_201), int64(7_400_000_202),
		int64(7_400_000_203), int64(7_400_000_204), int64(7_400_000_205)
	clearQueryIDs(t, ctx, pool, flips, none)
	step := time.Date(2025, 8, 1, 12, 0, 0, 0, time.UTC)
	for _, s := range []struct {
		qid  int64
		at   time.Duration
		hash *string
	}{
		{flips, -time.Hour, strPtr("A")}, {flips, -time.Minute, strPtr("B")},
		{flips, 0, nil}, {flips, 30 * time.Second, strPtr("B")},
		{flips, time.Minute, strPtr("C")}, {flips, 2 * time.Minute, strPtr("D")},
		{steady, -time.Minute, strPtr("S")}, {steady, time.Minute, strPtr("S")},
		{late, -time.Minute, strPtr("L")}, {late, 2 * time.Hour, strPtr("M")},
		{after, time.Minute, strPtr("N")}, {none, time.Minute, nil},
	} {
		insertHash(t, ctx, pool, s.qid, step.Add(s.at), s.hash)
	}
	got, err := PlanFlips(ctx, pool, []int64{flips, steady, late, after, none}, step,
		step.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]PlanFlip{
		flips: {QueryID: flips, BeforeAt: step.Add(-time.Minute), Before: "B",
			AfterAt: step.Add(time.Minute), After: "C"},
		steady: {QueryID: steady, BeforeAt: step.Add(-time.Minute), Before: "S"},
		late:   {QueryID: late, BeforeAt: step.Add(-time.Minute), Before: "L"},
		after:  {QueryID: after, AfterAt: step.Add(time.Minute), After: "N"},
	}
	if len(got) != len(want) {
		t.Fatalf("PlanFlips = %+v, want %+v", got, want)
	}
	for id, w := range want {
		if g := got[id]; g != w {
			t.Errorf("query %d: %+v, want %+v", id, g, w)
		}
	}
}

// Nil/empty and invalid input: no ids reads nothing; an inverted window
// is an error naming it.
func TestPlanFlipsEmptyAndInvertedWindow(t *testing.T) {
	step := time.Date(2025, 8, 1, 12, 0, 0, 0, time.UTC)
	got, err := PlanFlips(context.Background(), nil, nil, step, step)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("no ids: %v, %v", got, err)
	}
	if _, err := PlanFlips(context.Background(), nil, []int64{1}, step,
		step.Add(-time.Second)); err == nil {
		t.Fatal("inverted window accepted")
	}
}

// Error propagation: a store that cannot run the read is an error with
// the query count, never an empty result.
func TestPlanFlipsWithoutDatabaseIsAnError(t *testing.T) {
	step := time.Date(2025, 8, 1, 12, 0, 0, 0, time.UTC)
	if got, err := PlanFlips(context.Background(), nil, []int64{1, 2}, step,
		step.Add(time.Hour)); err == nil || got != nil {
		t.Fatalf("nil database: %v, %v", got, err)
	}
}
