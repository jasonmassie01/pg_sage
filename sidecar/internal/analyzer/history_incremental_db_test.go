package analyzer

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// Performance gate offender 4 (v1.8.3): the regression baseline rebuilt
// up to maxHistorySamples delta snapshots in PL/pgSQL on every analyzer
// cycle (131-256 ms a call; ~1.35 ms per 60-element document, linear in
// the number of statements). The sample is now one snapshot per
// epoch-aligned time bucket (lookback / maxHistorySamples wide), every
// snapshot when there are no more than maxHistorySamples, so the picks
// are stable from one cycle to the next and each is decoded once: a
// steady cycle decodes only snapshots it has not seen.

// recordingAnalyzer is an analyzer over its own pool whose statements are
// recorded, so a test can count the snapshots it decodes.
func recordingAnalyzer(t *testing.T, lookbackDays int) (*Analyzer, *testdb.QueryRecorder) {
	t.Helper()
	phase2Pool(t) // bootstrap and cross-package serialization
	cfg, err := pgxpool.ParseConfig(os.Getenv("SAGE_DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	rec := &testdb.QueryRecorder{}
	cfg.ConnConfig.Tracer = rec
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	c := phase2Config()
	c.Analyzer.RegressionLookbackDays = lookbackDays
	return New(pool, c, nil, nil, nil, nil, nil, noopLog), rec
}

// decodedSnapshots counts the snapshots the recorded statements rebuilt
// through sage.snapshot_data: every such statement passes the ids it
// decodes as its first argument.
func decodedSnapshots(t *testing.T, rec *testdb.QueryRecorder) int {
	t.Helper()
	n := 0
	for _, q := range rec.Matching("sage.snapshot_data(") {
		if len(q.Args) == 0 {
			t.Fatalf("decode statement without ids: %s", q.SQL)
		}
		ids, ok := q.Args[0].([]int64)
		if !ok {
			t.Fatalf("decode statement's first argument is %T, want []int64", q.Args[0])
		}
		n += len(ids)
	}
	rec.Reset()
	return n
}

// bucketStart is the start of the epoch-aligned bucket j buckets before
// the one holding now.
func bucketStart(now time.Time, width time.Duration, j int) time.Time {
	return now.Truncate(width).Add(-time.Duration(j) * width)
}

func TestHistoricalAverages_FirstSnapshotPerBucketDecodedOnce(t *testing.T) {
	const lookbackDays, buckets = 4, 60
	a, rec := recordingAnalyzer(t, lookbackDays)
	pool := phase2Pool(t)
	cleanQuerySnapshots(t, pool)
	t.Cleanup(func() { cleanQuerySnapshots(t, pool) })
	width := time.Duration(lookbackDays) * 24 * time.Hour / maxHistorySamples
	now := time.Now().UTC()
	// Two snapshots per bucket, 120 in all (more than maxHistorySamples):
	// the first (mean j) is the bucket's sample, the second (1000) never.
	for j := 1; j <= buckets; j++ {
		b := bucketStart(now, width, j)
		insertQuerySnapshot(t, pool, b.Add(time.Second), []map[string]any{
			{"queryid": 7, "mean_exec_time": float64(j)}})
		insertQuerySnapshot(t, pool, b.Add(width/2), []map[string]any{
			{"queryid": 7, "mean_exec_time": 1000.0}})
	}
	ctx := context.Background()
	if got := a.buildHistoricalAverages(ctx)[7]; got != 30.5 {
		t.Fatalf("avg(7) = %v, want 30.5 (the first snapshot of each bucket)", got)
	}
	if n := decodedSnapshots(t, rec); n != buckets {
		t.Fatalf("cold cycle decoded %d snapshots, want %d (one per bucket)", n, buckets)
	}
	if got := a.buildHistoricalAverages(ctx)[7]; got != 30.5 {
		t.Fatalf("second cycle avg(7) = %v, want 30.5", got)
	}
	if n := decodedSnapshots(t, rec); n != 0 {
		t.Fatalf("a cycle with no new snapshot decoded %d, want 0", n)
	}
	insertQuerySnapshot(t, pool, bucketStart(now, width, buckets+1).Add(time.Second),
		[]map[string]any{{"queryid": 7, "mean_exec_time": float64(buckets + 1)}})
	if got := a.buildHistoricalAverages(ctx)[7]; got != 31 {
		t.Fatalf("avg(7) after a new bucket = %v, want 31", got)
	}
	if n := decodedSnapshots(t, rec); n != 1 {
		t.Fatalf("a cycle with one new snapshot decoded %d, want 1", n)
	}
}

// With no more than maxHistorySamples snapshots every one is used, and a
// snapshot that left the window no longer counts.
func TestHistoricalAverages_FewSnapshotsAllUsedAndForgotten(t *testing.T) {
	a, rec := recordingAnalyzer(t, 7)
	pool := phase2Pool(t)
	cleanQuerySnapshots(t, pool)
	t.Cleanup(func() { cleanQuerySnapshots(t, pool) })
	now := time.Now()
	for i, mean := range []float64{10, 20, 30} {
		insertQuerySnapshot(t, pool, now.Add(-time.Duration(i+1)*time.Minute),
			[]map[string]any{{"queryid": 11, "mean_exec_time": mean}})
	}
	ctx := context.Background()
	if got := a.buildHistoricalAverages(ctx)[11]; got != 20 {
		t.Fatalf("avg(11) = %v, want 20 (all three)", got)
	}
	if n := decodedSnapshots(t, rec); n != 3 {
		t.Fatalf("decoded %d, want 3", n)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM sage.snapshots WHERE category = 'queries'
		AND data @> '[{"mean_exec_time": 30}]'`); err != nil {
		t.Fatal(err)
	}
	if got := a.buildHistoricalAverages(ctx)[11]; got != 15 {
		t.Fatalf("avg(11) after a snapshot left = %v, want 15", got)
	}
	if n := decodedSnapshots(t, rec); n != 0 {
		t.Fatalf("decoded %d after a deletion, want 0", n)
	}
}

// The sample stays capped however long the history: a cold cycle over
// 1,500 snapshots decodes at most maxHistorySamples, each statement a
// bounded batch.
func TestHistoricalAverages_ColdCycleCapped(t *testing.T) {
	const days, perDay = 5, 300
	a, rec := recordingAnalyzer(t, days+1)
	pool := phase2Pool(t)
	seedHistory(t, pool, days, perDay)
	avgs := a.buildHistoricalAverages(context.Background())
	if avgs[8] != 50 || math.IsNaN(avgs[7]) {
		t.Fatalf("averages = %v, want queryid 8 at 50", avgs)
	}
	batches := rec.Matching("sage.snapshot_data(")
	n := decodedSnapshots(t, rec)
	if n < maxHistorySamples/2 || n > maxHistorySamples {
		t.Fatalf("cold cycle decoded %d of %d snapshots, want at most %d", n, days*perDay,
			maxHistorySamples)
	}
	for _, b := range batches {
		if ids := b.Args[0].([]int64); len(ids) > historyDecodeBatch {
			t.Fatalf("one statement decoded %d snapshots, want at most %d", len(ids),
				historyDecodeBatch)
		}
	}
}
