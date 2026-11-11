package verify

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// history.store: meta. The observation source reads its history
// (query_store intervals, system and tables snapshots) from the store and
// its catalog (pg_index, the watched table's name) from the monitored
// database: the two halves of what used to be one database. Results must
// equal monitored mode's on the same fixture.

func seedVerifyHistory(t *testing.T, p *histfixture.Pair, from time.Time) {
	t.Helper()
	ctx := context.Background()
	st := histstore.Resolve(p.Monitored)
	calls := []int64{100, 110, 130, 160, 170, 200}
	totals := []float64{1000, 1100, 1500, 1800, 1900, 2200}
	for i := range calls {
		at := from.Add(time.Duration(i)*20*time.Minute + time.Minute)
		if _, err := st.Exec(ctx, `INSERT INTO sage.query_store
			(captured_at, queryid, calls, total_exec_time, mean_exec_time{dbcol})
			VALUES ($1, 77, $2, $3, 0{dbval})`, at, calls[i], totals[i]); err != nil {
			t.Fatalf("seed sample: %v", err)
		}
		if _, err := st.Exec(ctx, `INSERT INTO sage.snapshots
			(collected_at, category, data{dbcol}) VALUES ($1, 'system',
			jsonb_build_object('wal_lsn', $2::int8, 'relation_stats_epoch',
			'2026-01-01T00:00:00Z'){dbval})`, at, int64(100_000*i*i)); err != nil {
			t.Fatalf("seed system snapshot: %v", err)
		}
		if _, err := st.Exec(ctx, `INSERT INTO sage.snapshots
			(collected_at, category, data{dbcol}) VALUES ($1, 'tables',
			jsonb_build_array(jsonb_build_object('schemaname', 'public', 'relname', 'vt',
			'n_tup_ins', $2::int8, 'n_tup_upd', 0, 'n_tup_del', 0)){dbval})`,
			at, int64(1000*i*i)); err != nil {
			t.Fatalf("seed tables snapshot: %v", err)
		}
	}
}

func TestObservationSourceIdenticalInBothPlacements(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	if _, err := p.Monitored.Exec(ctx, `CREATE TABLE IF NOT EXISTS public.vt (a int);
		CREATE INDEX IF NOT EXISTS vt_a ON public.vt (a)`); err != nil {
		t.Fatal(err)
	}
	to := time.Now().UTC().Truncate(time.Second)
	from := to.Add(-2 * time.Hour)
	type answer struct {
		q     Measurement
		w     WriteCost
		valid bool
	}
	got := map[histstore.Mode]answer{}
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		p.Noise(t, from.Add(30*time.Minute), 77)
		seedVerifyHistory(t, p, from)
		src := NewPostgresObservationSource(p.Monitored)
		ms, err := src.QueryMeasurements(ctx, []int64{77}, from, to)
		if err != nil {
			t.Fatalf("%s QueryMeasurements: %v", mode, err)
		}
		w, err := src.WriteCost(ctx, "public.vt", from, to)
		if err != nil {
			t.Fatalf("%s WriteCost: %v", mode, err)
		}
		valid, err := src.IndexValid(ctx, "public.vt_a")
		if err != nil {
			t.Fatalf("%s IndexValid: %v", mode, err)
		}
		got[mode] = answer{q: ms[77], w: w, valid: valid}
	}
	mon, meta := got[histstore.ModeMonitored], got[histstore.ModeMeta]
	if mon.q.Samples != 100 || mon.q.Buckets != 5 {
		t.Fatalf("fixture: monitored measurement %+v, want 100 calls in 5 buckets", mon.q)
	}
	if mon != meta {
		t.Fatalf("observations differ:\nmonitored %+v\nmeta      %+v", mon, meta)
	}
	if mon.w.Intervals != 5 || mon.w.BytesPerTuple != 100 || mon.w.Tuples != 25_000 {
		t.Fatalf("write cost = %+v, want 5 intervals of the 6 cycles at 100 bytes/tuple",
			mon.w)
	}
	if !meta.valid {
		t.Fatal("meta mode must read pg_index on the monitored database (the index " +
			"exists only there)")
	}
}
