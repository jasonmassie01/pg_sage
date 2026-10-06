package verify

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// history.store: meta. The observation source reads its history
// (query_store intervals, system snapshots) from the store and its catalog
// (pg_index) from the monitored database: the two halves of what used to
// be one database. Results must equal monitored mode's on the same fixture.

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
			jsonb_build_object('blk_write_time', $2::float8){dbval})`,
			at, float64(50*i*i)); err != nil {
			t.Fatalf("seed system snapshot: %v", err)
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
		w     Measurement
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
		w, err := src.WriteMeasurements(ctx, "public.vt", from, to)
		if err != nil {
			t.Fatalf("%s WriteMeasurements: %v", mode, err)
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
	if mon.w.Samples != 6 {
		t.Fatalf("write measurement samples = %d, want the 6 system snapshots", mon.w.Samples)
	}
	if !meta.valid {
		t.Fatal("meta mode must read pg_index on the monitored database (the index " +
			"exists only there)")
	}
}
