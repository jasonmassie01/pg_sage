package querystore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// rules-05: the regression baseline reads many queries' hourly intervals
// in one statement. Each query's window follows the same anchor and epoch
// rules as QueryIntervals (written once, in intervals.go).

// No concurrent access tests: QueryIntervalsMany is one read-only statement
// with no shared state; the registry it resolves through has its own
// concurrency test (histstore).

// Parity: for every verify golden window, each query's intervals equal
// what QueryIntervals answers for it alone, and a second query with
// different counters in the same window does not leak into the first.
func TestRegressionIntervalsManyMatchesQueryIntervals(t *testing.T) {
	pool, ctx := requireDB(t)
	defer pool.Close()
	a := time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)
	const qid, other = int64(7_400_000_101), int64(7_400_000_102)
	for i, tc := range verifyGoldenCases(qid, a, a.Add(24*time.Hour)) {
		t.Run(tc.name, func(t *testing.T) {
			clearQueryIDs(t, ctx, pool, qid, other)
			to := time.Date(2025, 7, 1+i, 12, 0, 0, 0, time.UTC)
			from := to.Add(-30 * time.Minute)
			seedRows(t, ctx, pool, to, tc.rows)
			for _, r := range tc.rows {
				insertAt(t, ctx, pool, other, to.Add(r.at), r.calls*3, r.total*7, r.epoch)
			}
			got, err := QueryIntervalsMany(ctx, pool, []int64{qid, other, qid}, from, to,
				5*time.Minute)
			if err != nil {
				t.Fatalf("QueryIntervalsMany: %v", err)
			}
			for _, id := range []int64{qid, other} {
				want, err := QueryIntervals(ctx, pool, id, from, to, 5*time.Minute)
				if err != nil {
					t.Fatalf("QueryIntervals(%d): %v", id, err)
				}
				if !sameIntervals(got[id], want) {
					t.Fatalf("query %d: many = %+v, single = %+v", id, got[id], want)
				}
			}
			calls, broken := intervalTotals(got[qid])
			if len(got[qid]) != tc.wantN || calls != tc.wantCalls || broken != tc.wantBroken {
				t.Fatalf("intervals = %+v; want %d buckets, %d calls, broken=%v",
					got[qid], tc.wantN, tc.wantCalls, tc.wantBroken)
			}
		})
	}
}

// Hourly buckets over a seeded day: one interval per hour, oldest first,
// each with that hour's calls and time.
func TestRegressionIntervalsManyHourlyBuckets(t *testing.T) {
	pool, ctx := requireDB(t)
	defer pool.Close()
	const qid = int64(7_400_000_111)
	clearQueryIDs(t, ctx, pool, qid, qid)
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	start := time.Date(2025, 8, 1, 0, 0, 0, 0, time.UTC)
	for h := 0; h <= 24; h++ {
		insertAt(t, ctx, pool, qid, start.Add(time.Duration(h)*time.Hour+time.Minute),
			int64(100*h), float64(200*h)+float64(h*h), &epoch)
	}
	got, err := QueryIntervalsMany(ctx, pool, []int64{qid}, start, start.Add(24*time.Hour+
		30*time.Minute), time.Hour)
	if err != nil {
		t.Fatalf("QueryIntervalsMany: %v", err)
	}
	ivs := got[qid]
	if len(ivs) != 24 {
		t.Fatalf("buckets = %d (%+v), want 24", len(ivs), ivs)
	}
	for i, iv := range ivs {
		h := float64(i + 1)
		wantMs := 200 + h*h - (h-1)*(h-1)
		if iv.Calls != 100 || iv.TotalMs != wantMs || iv.EpochBreak {
			t.Fatalf("bucket %d = %+v, want 100 calls, %v ms, no break", i, iv, wantMs)
		}
	}
}

// Nil/empty: no ids reads nothing (not even the database) and returns an
// empty map; ids without samples are absent from the result.
func TestRegressionIntervalsManyEmpty(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	for _, ids := range [][]int64{nil, {}} {
		got, err := QueryIntervalsMany(ctx, nil, ids, now.Add(-time.Hour), now, time.Hour)
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("ids %v: got %v, %v; want an empty map and no read", ids, got, err)
		}
	}
	pool, ctx := requireDB(t)
	defer pool.Close()
	got, err := QueryIntervalsMany(ctx, pool, []int64{-99_887_766, -99_887_767},
		now.Add(-time.Hour), now, time.Minute)
	if err != nil || len(got) != 0 {
		t.Fatalf("unknown ids = %+v, %v; want no intervals", got, err)
	}
}

func TestRegressionIntervalsManyRejectsInvalidInput(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	for _, bucket := range []time.Duration{0, -time.Second} {
		_, err := QueryIntervalsMany(ctx, histstore.NewMonitored(nil), []int64{1},
			now.Add(-time.Hour), now, bucket)
		if err == nil || !strings.Contains(err.Error(), "bucket") {
			t.Fatalf("bucket %v: err = %v, want a bucket error", bucket, err)
		}
	}
	_, err := QueryIntervalsMany(ctx, nil, []int64{1}, now.Add(-time.Hour), now, time.Hour)
	if !errors.Is(err, histstore.ErrNoDatabase) {
		t.Fatalf("nil database: err = %v, want ErrNoDatabase", err)
	}
	if err == nil || !strings.Contains(err.Error(), "1 queries") {
		t.Fatalf("error must say what was read: %v", err)
	}
	_, err = QueryIntervalsMany(ctx, histstore.NewMonitored(nil), []int64{1},
		now, now.Add(-time.Hour), time.Hour)
	if err == nil || !strings.Contains(err.Error(), "window") {
		t.Fatalf("reversed window: err = %v, want a window error", err)
	}
}

// Meta mode: the reader scopes to its database's rows; another database's
// rows at the same times and query ids change nothing.
func TestRegressionIntervalsManyAgreesInBothHistoryModes(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	const qid = int64(7_400_000_121)
	cur := time.Date(2025, 9, 1, 12, 0, 0, 0, time.UTC)
	seed := []seedRow{{qid, -20 * time.Minute, 100, 1000, nil},
		{qid, -10 * time.Minute, 150, 2000, nil}, {qid, 0, 180, 2600, nil}}
	got := map[histstore.Mode][]Interval{}
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		target, id := p.Monitored, 0
		if mode == histstore.ModeMeta {
			target, id = p.Meta, histfixture.DatabaseID
			p.Noise(t, cur, qid)
			p.Noise(t, cur.Add(-10*time.Minute), qid)
		}
		for _, r := range seed {
			insertScoped(t, ctx, target, r.qid, cur.Add(r.at), r.calls, r.total, id)
		}
		ivs, err := QueryIntervalsMany(ctx, p.Monitored, []int64{qid},
			cur.Add(-15*time.Minute), cur, time.Minute)
		if err != nil {
			t.Fatalf("%s QueryIntervalsMany: %v", mode, err)
		}
		got[mode] = ivs[qid]
	}
	mon, meta := got[histstore.ModeMonitored], got[histstore.ModeMeta]
	if calls, _ := intervalTotals(mon); calls != 80 || len(mon) != 2 {
		t.Fatalf("monitored intervals = %+v, want 2 buckets of 80 calls", mon)
	}
	if !sameIntervals(mon, meta) {
		t.Fatalf("intervals differ: monitored %+v, meta %+v", mon, meta)
	}
}
