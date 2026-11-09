package analyzer

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// rules-04 on real PostgreSQL: sage.unused_index_clock round trips, one
// write per cycle, fleet isolation, load retry and the history backfill.
// Each test owns a database (testdb.CreateDatabase), so the shared test
// database's open findings never leak into the clock.

// clockDB is a fresh bootstrapped database and a recording pool on it.
func clockDB(t *testing.T, label string) (*pgxpool.Pool, *pgxpool.Pool, *testdb.QueryRecorder) {
	t.Helper()
	ctx := context.Background()
	base, err := pgxpool.New(ctx, testdb.CreateDatabase(t, label))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(base.Close)
	if err := schema.Bootstrap(ctx, base); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	cfg := base.Config().Copy()
	rec := &testdb.QueryRecorder{}
	cfg.ConnConfig.Tracer = rec
	recPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect recording pool: %v", err)
	}
	t.Cleanup(recPool.Close)
	return base, recPool, rec
}

func clockAnalyzer(pool *pgxpool.Pool, now *time.Time) *Analyzer {
	a := New(pool, phase2Config(), nil, nil, nil, nil, nil, noopLog)
	a.extras.Now = func() time.Time { return *now }
	return a
}

// dbClockCycle runs the rule, persistence, resolution and clock save of
// one cycle on snap.
func dbClockCycle(t *testing.T, a *Analyzer, snap *collector.Snapshot) []Finding {
	t.Helper()
	ctx := context.Background()
	a.eval = newCycleEval()
	a.prepareUnusedClock(ctx, snap)
	var unused []Finding
	for _, f := range a.runSnapshotRules(snap, nil, false) {
		if f.Category == "unused_index" {
			unused = append(unused, f)
		}
	}
	a.finalizeCycle(ctx, unused, a.eval.resolvable(unused))
	a.saveUnusedClock(ctx)
	return unused
}

func clockIndexes(names map[string]uint32, scans int64) *collector.Snapshot {
	snap := &collector.Snapshot{}
	keys := make([]string, 0, len(names))
	for n := range names {
		keys = append(keys, n)
	}
	sort.Strings(keys)
	for _, n := range keys {
		snap.Indexes = append(snap.Indexes, collector.IndexStats{
			SchemaName: "public", RelName: "t_" + n, IndexRelName: n,
			IndexRelID: names[n], IdxScan: scans, IsValid: true,
			IndexDef: "CREATE INDEX " + n + " ON public.t_" + n + " USING btree (a)"})
	}
	return snap
}

type storedClock struct {
	oid   int64
	since time.Time
}

func readClockTable(t *testing.T, pool *pgxpool.Pool) map[string]storedClock {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT ident, indexrelid::int8,
		zero_since FROM sage.unused_index_clock`)
	if err != nil {
		t.Fatalf("read clock table: %v", err)
	}
	defer rows.Close()
	out := map[string]storedClock{}
	for rows.Next() {
		var ident string
		var c storedClock
		if err := rows.Scan(&ident, &c.oid, &c.since); err != nil {
			t.Fatalf("scan clock row: %v", err)
		}
		out[ident] = c
	}
	return out
}

// clockWrites counts the recorded statements that write the clock table.
func clockWrites(rec *testdb.QueryRecorder) int {
	n := 0
	for _, q := range rec.Matching("sage.unused_index_clock") {
		up := strings.ToUpper(q.SQL)
		if strings.Contains(up, "INSERT") || strings.Contains(up, "DELETE") ||
			strings.Contains(up, "UPDATE") {
			n++
		}
	}
	rec.Reset()
	return n
}

// CHECK-16 and the happy path on a real table: the first sightings are
// written in one statement, a steady cycle writes nothing, a scan deletes
// the forgotten row, and a restarted analyzer resumes the clock.
func TestUnusedClockDB_RoundTripOneWritePerCycle(t *testing.T) {
	base, pool, rec := clockDB(t, "unused_clock_rt")
	now := time.Now().UTC().Truncate(time.Microsecond)
	start := now
	a := clockAnalyzer(pool, &now)
	both := map[string]uint32{"ix_a": 100, "ix_b": 101}
	dbClockCycle(t, a, clockIndexes(both, 0))
	if n := clockWrites(rec); n != 1 {
		t.Fatalf("first cycle wrote the clock %d times, want 1", n)
	}
	stored := readClockTable(t, base)
	if len(stored) != 2 || stored["public.ix_a"].oid != 100 ||
		!stored["public.ix_b"].since.Equal(start) {
		t.Fatalf("stored clocks = %+v, want both at %s", stored, start)
	}
	now = now.Add(time.Minute)
	dbClockCycle(t, a, clockIndexes(both, 0))
	if n := clockWrites(rec); n != 0 {
		t.Fatalf("steady cycle wrote the clock %d times, want 0", n)
	}
	now = start.Add(8 * day)
	b := clockAnalyzer(pool, &now) // restart
	got := dbClockCycle(t, b, clockIndexes(map[string]uint32{"ix_a": 100}, 0))
	if len(got) != 1 || got[0].Detail["unused_since"] != start.Format(time.RFC3339) {
		t.Fatalf("restarted analyzer: %+v, want ix_a unused since %s", got, start)
	}
	if n := clockWrites(rec); n > 1 {
		t.Fatalf("restart cycle wrote the clock %d times, want at most 1", n)
	}
	if stored := readClockTable(t, base); len(stored) != 1 || stored["public.ix_a"].oid != 100 {
		t.Fatalf("dropped ix_b's row not removed: %+v", stored)
	}
}

// Concurrency: two analyzers on two databases (fleet mode, a sage schema
// per database) write their own tables at the same time without
// cross-talk. -race is unavailable on this machine (no cgo).
func TestUnusedClockDB_FleetDatabasesIsolated(t *testing.T) {
	baseA, poolA, _ := clockDB(t, "unused_clock_fa")
	baseB, poolB, _ := clockDB(t, "unused_clock_fb")
	var wg sync.WaitGroup
	run := func(pool *pgxpool.Pool, prefix string) {
		defer wg.Done()
		now := time.Now().UTC()
		a := clockAnalyzer(pool, &now)
		for i := 0; i < 20; i++ {
			names := map[string]uint32{}
			for j := 0; j <= i%4; j++ {
				names[fmt.Sprintf("%s_%d", prefix, j)] = uint32(1000 + j)
			}
			a.eval = newCycleEval()
			ctx := context.Background()
			a.prepareUnusedClock(ctx, clockIndexes(names, 0))
			a.runSnapshotRules(clockIndexes(names, 0), nil, false)
			a.saveUnusedClock(ctx)
			now = now.Add(time.Minute)
		}
	}
	wg.Add(2)
	go run(poolA, "fa")
	go run(poolB, "fb")
	wg.Wait()
	for name, base := range map[string]*pgxpool.Pool{"fa": baseA, "fb": baseB} {
		stored := readClockTable(t, base)
		if len(stored) != 4 {
			t.Fatalf("%s: %d clock rows, want 4: %+v", name, len(stored), stored)
		}
		for ident := range stored {
			if !strings.HasPrefix(ident, "public."+name+"_") {
				t.Fatalf("%s: foreign clock row %s", name, ident)
			}
		}
	}
}

// Error propagation on a real table: a missing table fails the category
// and the load is retried once the migration has run again.
func TestUnusedClockDB_MissingTableRetriedAfterBootstrap(t *testing.T) {
	base, pool, _ := clockDB(t, "unused_clock_missing")
	ctx := context.Background()
	if _, err := base.Exec(ctx, `DROP TABLE sage.unused_index_clock`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	now := time.Now().UTC()
	a := clockAnalyzer(pool, &now)
	snap := clockIndexes(map[string]uint32{"ix_m": 300}, 0)
	a.eval = newCycleEval()
	a.prepareUnusedClock(ctx, snap)
	a.runSnapshotRules(snap, nil, false)
	if a.eval.resolvable(nil)["unused_index"] {
		t.Fatalf("missing clock table left unused_index resolvable")
	}
	a.saveUnusedClock(ctx) // must not try to write while unloaded
	if err := schema.Bootstrap(ctx, base); err != nil {
		t.Fatalf("re-bootstrap: %v", err)
	}
	now = now.Add(time.Minute)
	dbClockCycle(t, a, snap)
	if !a.eval.resolvable(nil)["unused_index"] {
		t.Fatalf("load not retried after the table came back")
	}
	if stored := readClockTable(t, base); stored["public.ix_m"].oid != 300 {
		t.Fatalf("clock not saved after the retry: %+v", stored)
	}
}

func insertIndexSnapshot(t *testing.T, pool *pgxpool.Pool, at time.Time,
	idx []collector.IndexStats) {
	t.Helper()
	raw, err := json.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO sage.snapshots
		(collected_at, category, data) VALUES ($1, 'indexes', $2::jsonb)`,
		at, string(raw)); err != nil {
		t.Fatalf("insert indexes snapshot: %v", err)
	}
}

func openFindingID(t *testing.T, pool *pgxpool.Pool, ident string) (int64, int) {
	t.Helper()
	var id int64
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT id, occurrence_count
		FROM sage.findings WHERE category = 'unused_index' AND status = 'open'
		  AND object_identifier = $1`, ident).Scan(&id, &n); err != nil {
		t.Fatalf("open finding %s: %v", ident, err)
	}
	return id, n
}

// One-time backfill: an open finding from before the clock table and
// eight daily 'indexes' documents at zero scans. The first cycle resumes
// the clock from the oldest day and refreshes the same finding row.
func TestUnusedClockDB_BackfillFromHistoryKeepsFindingID(t *testing.T) {
	base, pool, _ := clockDB(t, "unused_clock_hist")
	now := time.Now().UTC().Truncate(time.Second)
	snap := clockIndexes(map[string]uint32{"ix_h": 400}, 0)
	for d := 0; d <= 7; d++ {
		insertIndexSnapshot(t, base, now.Add(-time.Duration(d)*day-time.Hour), snap.Indexes)
	}
	old := unusedIndexFinding(snap.Indexes[0], "public.ix_h", nil, 7)
	if err := UpsertFindings(context.Background(), base, []Finding{old}); err != nil {
		t.Fatalf("seed open finding: %v", err)
	}
	id, count := openFindingID(t, base, "public.ix_h")
	a := clockAnalyzer(pool, &now)
	got := dbClockCycle(t, a, snap)
	oldest := now.Add(-7*day - time.Hour)
	if len(got) != 1 || got[0].Detail["unused_since"] != oldest.Format(time.RFC3339) {
		t.Fatalf("backfill: %+v, want unused since %s", got, oldest.Format(time.RFC3339))
	}
	if id2, count2 := openFindingID(t, base, "public.ix_h"); id2 != id || count2 <= count {
		t.Fatalf("finding id %d->%d occurrence %d->%d, want same id and a higher count",
			id, id2, count, count2)
	}
	if stored := readClockTable(t, base); !stored["public.ix_h"].since.Equal(oldest) {
		t.Fatalf("backfilled clock = %+v, want %s", stored["public.ix_h"], oldest)
	}
}
