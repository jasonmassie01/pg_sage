package api

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/snapstore"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/snapfixture"
)

// Snapshot dedupe golden comparison for the snapshot API (the history
// export): the same scenario written in the legacy format and through the
// delta writer gives identical latest documents and history points.

func dedupeLegacyPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "api_snap_legacy"))
	if err != nil {
		t.Fatalf("connect legacy store: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap legacy store: %v", err)
	}
	return pool
}

// The dedupe scenario: dedupeCycles collection cycles dedupeStep apart.
const (
	dedupeCycles = 30
	dedupeStep   = 5 * time.Minute
)

// dedupeStart is when the scenario's first cycle is collected, with the
// clock at now.
func dedupeStart(now time.Time) time.Time {
	return now.UTC().Add(-3 * time.Hour).Truncate(time.Minute)
}

func dedupeScenario(t *testing.T, now time.Time) []snapfixture.Cycle {
	t.Helper()
	sc := snapfixture.Scenario{
		Start:   dedupeStart(now),
		Step:    dedupeStep,
		Cycles:  dedupeCycles,
		Tables:  12,
		Indexes: 60,
		Seed:    7,
		Events: snapfixture.Events{CounterReset: 5, DropIndex: 8, Redefine: 12, Rename: 15,
			EmptyFrom: 18, EmptyTo: 20, Unavailable: 22, QueryChurn: 3},
	}
	cycles, err := sc.Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return cycles
}

func seedDedupeStores(t *testing.T, now time.Time) (*pgxpool.Pool, *pgxpool.Pool,
	context.Context, []snapfixture.Cycle) {
	t.Helper()
	deltaPool, ctx := phase2RequireDB(t)
	if _, err := deltaPool.Exec(ctx, "DELETE FROM sage.snapshots"); err != nil {
		t.Fatalf("clean snapshots: %v", err)
	}
	t.Cleanup(func() { _, _ = deltaPool.Exec(ctx, "DELETE FROM sage.snapshots") })
	legacyPool := dedupeLegacyPool(t, ctx)
	cycles := dedupeScenario(t, now)
	w := snapstore.NewWriter()
	for _, c := range cycles {
		rows := make([]snapstore.Row, 0, len(c.Docs))
		for _, d := range c.Docs {
			rows = append(rows, snapstore.Row{Category: d.Category, Data: d.Data})
		}
		if err := w.Persist(ctx, deltaPool, c.At, rows); err != nil {
			t.Fatalf("persist delta: %v", err)
		}
		if err := snapfixture.InsertLegacy(ctx, legacyPool, c); err != nil {
			t.Fatalf("persist legacy: %v", err)
		}
	}
	return deltaPool, legacyPool, ctx, cycles
}

var dedupeMetrics = []string{"indexes", "tables", "sequences", "queries",
	"foreign_keys", "partitions", "system", "locks", "config_data"}

// Latest and history (sliding window and explicit range) are identical
// for every category, and the delta store really holds deltas.
func TestSnapshotAPI_DedupeGolden(t *testing.T) {
	deltaPool, legacyPool, ctx, cycles := seedDedupeStores(t, time.Now())
	var deltas int
	if err := deltaPool.QueryRow(ctx, `SELECT count(*) FROM sage.snapshots
		WHERE base_id IS NOT NULL`).Scan(&deltas); err != nil || deltas == 0 {
		t.Fatalf("delta rows = %d (%v), want some", deltas, err)
	}
	from, to := cycles[3].At, cycles[25].At
	for _, metric := range dedupeMetrics {
		gotL, err1 := querySnapshotLatest(ctx, deltaPool, metric)
		wantL, err2 := querySnapshotLatest(ctx, legacyPool, metric)
		if err1 != nil || err2 != nil || !reflect.DeepEqual(gotL, wantL) {
			t.Errorf("%s latest differs (%v / %v)", metric, err1, err2)
		}
		for _, window := range []struct{ from, to time.Time }{{}, {from, to}} {
			got, _, err1 := querySnapshotHistory(ctx, deltaPool, metric, 24, window.from, window.to)
			want, _, err2 := querySnapshotHistory(ctx, legacyPool, metric, 24, window.from,
				window.to)
			if err1 != nil || err2 != nil {
				t.Fatalf("%s history: %v / %v", metric, err1, err2)
			}
			if len(want) == 0 || !reflect.DeepEqual(got, want) {
				t.Errorf("%s history %v-%v differs: %d vs %d points", metric, window.from,
					window.to, len(got), len(want))
			}
		}
	}
}

// lastUTC0037 is the most recent 00:37 UTC. With the clock there, a
// scenario starting three hours back collects its last cycle at 00:02, the
// first document of a new UTC day, which the writer stores as a keyframe
// (a base never spans a day). Every CI failure of the orphan test (10-05,
// 10-08 in seven jobs, 10-10) was in a run that reached it at about 00:40 UTC.
func lastUTC0037(now time.Time) time.Time {
	at := now.UTC().Truncate(24 * time.Hour).Add(37 * time.Minute)
	if at.After(now) {
		at = at.Add(-24 * time.Hour)
	}
	return at
}

// The scenario fits in one UTC day, in the past and inside the snapshot
// API's 24-hour window, whatever the time of day: the writer starts a new
// keyframe with the first document of each day, so a scenario that ends
// just after midnight ends on a keyframe, not on a delta.
func TestDedupeScenarioStaysInOneUTCDay(t *testing.T) {
	day := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	span := time.Duration(dedupeCycles-1) * dedupeStep
	for m := 0; m < 24*60; m++ {
		now := day.Add(time.Duration(m)*time.Minute + 17*time.Second)
		first := dedupeStart(now)
		last := first.Add(span)
		if !first.Truncate(24 * time.Hour).Equal(last.Truncate(24 * time.Hour)) {
			t.Fatalf("clock %s: scenario %s to %s crosses a UTC midnight", now.Format("15:04:05"),
				first.Format("15:04"), last.Format("15:04"))
		}
		if !last.Before(now) || first.Before(now.Add(-24*time.Hour)) {
			t.Fatalf("clock %s: scenario %s to %s is not inside the last 24 hours",
				now.Format("15:04:05"), first, last)
		}
	}
}

// A delta row whose keyframe was deleted by hand reads as a null document,
// never as an error or a wrong document. Run at the current time and at
// 00:37 UTC (see lastUTC0037).
func TestSnapshotAPI_OrphanDeltaReadsNull(t *testing.T) {
	now := time.Now()
	for name, clock := range map[string]time.Time{"now": now,
		"00:37 UTC": lastUTC0037(now)} {
		t.Run(name, func(t *testing.T) { requireOrphanDeltaReadsNull(t, clock, name == "now") })
	}
}

// requireOrphanDeltaReadsNull: with sliding, history is read over the API's
// last-24-hours window, else from the scenario's first cycle on (a clock
// at the last 00:37 UTC can be more than 24 hours back).
func requireOrphanDeltaReadsNull(t *testing.T, clock time.Time, sliding bool) {
	t.Helper()
	deltaPool, _, ctx, cycles := seedDedupeStores(t, clock)
	from := cycles[0].At
	if sliding {
		from = time.Time{}
	}
	var newest int64
	var baseID *int64
	if err := deltaPool.QueryRow(ctx, `SELECT id, base_id FROM sage.snapshots
		WHERE category = 'indexes' ORDER BY collected_at DESC LIMIT 1`).
		Scan(&newest, &baseID); err != nil {
		t.Fatalf("find the newest indexes row: %v", err)
	}
	if baseID == nil {
		t.Fatalf("the newest indexes row %d is a keyframe: the scenario must end on a delta",
			newest)
	}
	if _, err := deltaPool.Exec(ctx, `DELETE FROM sage.snapshots WHERE id = $1`,
		*baseID); err != nil {
		t.Fatalf("delete keyframe: %v", err)
	}
	latest, err := querySnapshotLatest(ctx, deltaPool, "indexes")
	if err != nil || latest != nil {
		t.Fatalf("latest = %v (%v), want null without error", latest, err)
	}
	points, _, err := querySnapshotHistory(ctx, deltaPool, "indexes", 24, from, time.Time{})
	if err != nil || len(points) == 0 {
		t.Fatalf("history = %d points (%v)", len(points), err)
	}
	if last := points[len(points)-1]; string(last.Data) != "null" {
		t.Fatalf("orphan point = %s, want null data", last.Data)
	}
}
