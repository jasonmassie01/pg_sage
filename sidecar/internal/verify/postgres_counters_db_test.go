package verify

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/snapstore"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// CounterSource (lab-01a) reads the snapshot queries category through
// querystore.SnapshotIntervals and the seasonal history from the hourly
// rollup, both in the database's history store.

var counterIDs = []int64{501, 502, 503}

// persistCounterCycles writes n+1 one-minute cycles from t0 with the
// statements epoch changing at cycle breakAt (never when breakAt <= 0).
func persistCounterCycles(t *testing.T, db snapstore.Beginner, t0 time.Time, n,
	breakAt int) {
	t.Helper()
	w := snapstore.NewWriter()
	epoch := t0.Add(-24 * time.Hour)
	calls, hit, read := map[int64]int64{}, map[int64]int64{}, map[int64]int64{}
	for i := 0; i <= n; i++ {
		if i == breakAt {
			epoch = t0.Add(time.Duration(i) * time.Minute)
		}
		docs := make([]map[string]any, 0, len(counterIDs))
		for j, id := range counterIDs {
			calls[id] += int64(5 + (i+j)%3)
			hit[id] += int64(40 + 7*((i*j)%5))
			read[id] += int64(i % (j + 2))
			docs = append(docs, map[string]any{"queryid": id, "query": "SELECT fixture",
				"calls": calls[id], "total_exec_time": float64(calls[id]) * 1.5,
				"mean_exec_time": 1.5, "rows": calls[id], "shared_blks_hit": hit[id],
				"shared_blks_read": read[id], "temp_blks_read": 0, "temp_blks_written": 0,
				"wal_bytes": 0})
		}
		sys := map[string]any{"active_backends": 1,
			"statements_epoch": epoch.UTC().Format(time.RFC3339Nano)}
		at := t0.Add(time.Duration(i) * time.Minute)
		if err := w.Persist(context.Background(), db, at, []snapstore.Row{
			{Category: "queries", Data: mustMarshal(t, docs)},
			{Category: "system", Data: mustMarshal(t, sys)},
		}); err != nil {
			t.Fatalf("persist cycle %d: %v", i, err)
		}
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// directSums is calls and blocks per queryid straight from
// sage.snapshot_data(): the last cumulative value minus the one before the
// window's first interval.
func directSums(t *testing.T, db any, from, to time.Time) map[int64][2]float64 {
	t.Helper()
	rows, err := histstore.Resolve(db).Query(context.Background(), `WITH q AS (
		SELECT r.queryid, r.calls, r.shared_blks_hit + r.shared_blks_read AS blks
		FROM sage.snapshots s CROSS JOIN LATERAL jsonb_to_recordset(`+
		snapstore.DataSQL("s")+`) AS r(queryid int8, calls int8, shared_blks_hit int8,
			shared_blks_read int8)
		WHERE {db:s} AND s.category = 'queries'
			AND s.collected_at >= $1::timestamptz - interval '1 minute'
			AND s.collected_at < $2)
		SELECT queryid, max(calls) - min(calls), max(blks) - min(blks)
		FROM q GROUP BY queryid`, from, to)
	if err != nil {
		t.Fatalf("direct sums: %v", err)
	}
	defer rows.Close()
	out := map[int64][2]float64{}
	for rows.Next() {
		var id, calls, blks int64
		if err := rows.Scan(&id, &calls, &blks); err != nil {
			t.Fatal(err)
		}
		out[id] = [2]float64{float64(calls), float64(blks)}
	}
	return out
}

// seedHourly writes weeks of hourly rows for queryid 501 at the hours of
// [from, to) shifted back 1..weeks weeks, plus rows the reader must skip:
// an epoch-broken hour, an hour outside the pattern and an unasked queryid.
func seedHourly(t *testing.T, db any, from, to time.Time, weeks int) int {
	t.Helper()
	st, n := histstore.Resolve(db), 0
	ins := func(id int64, hour time.Time, calls int64, brk bool) {
		if _, err := st.Exec(context.Background(), `INSERT INTO sage.query_rollup_hourly
			(queryid, hour, calls, exec_ms, rows, shared_blks_hit, shared_blks_read,
			 temp_blks_read, temp_blks_written, wal_bytes, epoch_break, pairs{dbcol})
			VALUES ($1, $2, $3::int8, $3::int8 * 2.0, $3::int8, $3::int8 * 10, $3::int8,
			 0, 0, 0, $4, 60{dbval})`,
			id, hour, calls, brk); err != nil {
			t.Fatalf("seed hourly row: %v", err)
		}
	}
	for k := 1; k <= weeks; k++ {
		for h := from.Truncate(time.Hour); h.Before(to); h = h.Add(time.Hour) {
			at := h.Add(-time.Duration(k) * week)
			ins(501, at, int64(100*k), false)
			ins(502, at, 1, k == 1)
			ins(999, at, 7, false)
			n++
		}
	}
	ins(501, from.Truncate(time.Hour).Add(-week+10*time.Hour), 3, false)
	return n
}

func TestCounterSourceMatchesDirectSumInBothPlacements(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	t0 := time.Now().UTC().Truncate(time.Hour).Add(-4 * time.Hour)
	from, to := t0.Add(30*time.Second), t0.Add(180*time.Minute+30*time.Second)
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		clearHourly(t, p)
		persistCounterCycles(t, p.Monitored, t0, 180, 0)
		want := directSums(t, p.Monitored, from, to)
		src := NewCounterSource(p.Monitored)
		got, err := src.Window(ctx, counterIDs, from, to, 5*time.Minute)
		if err != nil {
			t.Fatalf("%s Window: %v", mode, err)
		}
		if got.EpochBreak || len(want) != 3 {
			t.Fatalf("%s: epoch break %v, direct sums %v", mode, got.EpochBreak, want)
		}
		for _, id := range counterIDs {
			calls, blocks := seriesTotals(got.Series[id])
			if calls != want[id][0] || blocks != want[id][1] || len(got.Series[id]) != 36 {
				t.Fatalf("%s query %d: %v calls %v blocks in %d buckets, want %v in 36",
					mode, id, calls, blocks, len(got.Series[id]), want[id])
			}
		}
		seeded := seedHourly(t, p.Monitored, from, to, 4)
		hist, err := src.Seasonal(ctx, []int64{501, 502}, from, to, 4)
		if err != nil {
			t.Fatalf("%s Seasonal: %v", mode, err)
		}
		if len(hist[501]) != seeded || len(hist[502]) != seeded-4 || hist[999] != nil {
			t.Fatalf("%s seasonal: %d rows of 501 (want %d), %d of 502 (want %d without "+
				"the broken week), unasked 999 %v", mode, len(hist[501]), seeded,
				len(hist[502]), seeded-4, hist[999])
		}
		if b := hist[501][0]; b.Blocks != float64(b.Calls)*11 || b.TotalMs != 2*
			float64(b.Calls) {
			t.Fatalf("%s seasonal bucket %+v: blocks must be hit plus read", mode, b)
		}
	}
}

func clearHourly(t *testing.T, p *histfixture.Pair) {
	t.Helper()
	for _, pool := range []*pgxpool.Pool{p.Monitored, p.Meta} {
		if _, err := pool.Exec(context.Background(),
			"TRUNCATE sage.query_rollup_hourly"); err != nil {
			t.Fatalf("clear hourly rollup: %v", err)
		}
	}
}

func seriesTotals(bs []Bucket) (calls, blocks float64) {
	for _, b := range bs {
		calls += float64(b.Calls)
		blocks += b.Blocks
	}
	return calls, blocks
}

// An epoch change inside the window (a reset, or a Neon or Lakebase
// suspend and resume) is reported, so the attribution gives no delta.
func TestCounterSourceReportsEpochBreak(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	t0 := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	persistCounterCycles(t, p.Monitored, t0, 30, 15)
	got, err := NewCounterSource(p.Monitored).Window(context.Background(), counterIDs,
		t0.Add(30*time.Second), t0.Add(31*time.Minute), 0)
	if err != nil || !got.EpochBreak {
		t.Fatalf("Window: %+v, %v; want an epoch break", got.EpochBreak, err)
	}
}

func TestCounterSourceMissingSnapshotDataNamesIt(t *testing.T) {
	pool := verifyPoolOn(t, testdb.CreateDatabase(t, "verify_nosnapdata"))
	ctx := context.Background()
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := pool.Exec(ctx, `DROP FUNCTION sage.snapshot_data(jsonb, bigint,
		timestamptz) CASCADE`); err != nil {
		t.Fatalf("drop snapshot_data: %v", err)
	}
	now := time.Now().UTC()
	_, err := NewCounterSource(pool).Window(ctx, counterIDs, now.Add(-time.Hour), now, 0)
	if err == nil || !strings.Contains(err.Error(), "snapshot_data") {
		t.Fatalf("err = %v, want it to name snapshot_data", err)
	}
}

func TestCounterSourceRefusesUnregisteredPoolInMetaMode(t *testing.T) {
	pool := verifyIntegrationPool(t)
	if err := histstore.SetProcessMode(histstore.ModeMeta); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = histstore.SetProcessMode(histstore.ModeMonitored) })
	ctx, now := context.Background(), time.Now().UTC()
	src := NewCounterSource(pool)
	if _, err := src.Window(ctx, counterIDs, now.Add(-time.Hour), now,
		0); !errors.Is(err, histstore.ErrNotRegistered) {
		t.Fatalf("Window err = %v, want histstore.ErrNotRegistered", err)
	}
	if _, err := src.Seasonal(ctx, counterIDs, now.Add(-time.Hour), now,
		4); !errors.Is(err, histstore.ErrNotRegistered) {
		t.Fatalf("Seasonal err = %v, want histstore.ErrNotRegistered", err)
	}
}

func TestCounterSourceRefusesBadInput(t *testing.T) {
	now := time.Now()
	var nilSrc *CounterSource
	if _, err := nilSrc.Window(context.Background(), counterIDs, now.Add(-time.Hour), now,
		0); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("nil source: %v, want unavailable", err)
	}
	src := NewCounterSource(nil)
	if _, err := src.Seasonal(context.Background(), counterIDs, now, now,
		4); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("nil pool: %v, want unavailable", err)
	}
	src = &CounterSource{queryer: (*pgxpool.Pool)(nil)}
	if _, err := src.Window(context.Background(), counterIDs, now, now.Add(-time.Hour),
		0); err == nil || !strings.Contains(err.Error(), "window") {
		t.Fatalf("inverted window: %v, want a refusal naming the window", err)
	}
	got, err := src.Seasonal(context.Background(), nil, now.Add(-time.Hour), now, 4)
	if err != nil || len(got) != 0 {
		t.Fatalf("no queryids: %v %v, want an empty series without a read", got, err)
	}
}
