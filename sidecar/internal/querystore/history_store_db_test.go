package querystore

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// history.store: meta. The recorder used to stamp each sample's plan_hash
// with a join on the monitored database's sage.explain_cache; in meta mode
// that join is split: fingerprints are read from the monitored database,
// samples are written to the store. Every result must equal the monitored
// mode's on the same fixture, with another database's samples (same query
// ids) in the store.

func seedPlans(t *testing.T, p *histfixture.Pair) {
	t.Helper()
	ctx := context.Background()
	if _, err := p.Monitored.Exec(ctx, "TRUNCATE sage.explain_cache"); err != nil {
		t.Fatal(err)
	}
	for _, pl := range []struct {
		qid  int64
		hash string
		ago  time.Duration
	}{{11, "old11", 2 * time.Hour}, {11, "new11", time.Hour}, {22, "only22", time.Hour}} {
		if _, err := p.Monitored.Exec(ctx, `INSERT INTO sage.explain_cache (captured_at,
			queryid, plan_json, source, plan_hash) VALUES (now() - $3::interval, $1, '{}',
			'test', $2)`, pl.qid, pl.hash, pl.ago); err != nil {
			t.Fatalf("seed plan: %v", err)
		}
	}
}

func recordCycles(t *testing.T, p *histfixture.Pair, start time.Time) {
	t.Helper()
	ctx := context.Background()
	r := NewRecorder()
	for i := 0; i < 6; i++ {
		at := start.Add(time.Duration(i) * time.Minute)
		s := []Sample{
			{QueryID: 11, Calls: int64(100 * (i + 1)), TotalExecMs: float64(200 * (i + 1)),
				MeanExecMs: 2},
			{QueryID: 22, Calls: int64(5 * (i + 1)), TotalExecMs: float64(500 * (i + 1)),
				MeanExecMs: 100},
			{QueryID: 33, Calls: 7, TotalExecMs: 70, MeanExecMs: 10}, // idle after the first
		}
		if _, err := r.Record(ctx, p.Monitored, s, at); err != nil {
			t.Fatalf("record cycle %d: %v", i, err)
		}
		// captured_at defaults to now(): date the cycle's rows.
		if _, err := histstore.Resolve(p.Monitored).Exec(ctx, `UPDATE sage.query_store q
			SET captured_at = $1 WHERE q.captured_at > now() - interval '1 minute' AND {db:q}`,
			at); err != nil {
			t.Fatalf("date cycle %d: %v", i, err)
		}
	}
}

func storeSamples(t *testing.T, p *histfixture.Pair) []string {
	t.Helper()
	rows, err := histstore.Resolve(p.Monitored).Query(context.Background(),
		`SELECT concat_ws(' ', q.queryid, q.calls, q.total_exec_time, q.mean_exec_time,
		        q.plan_hash) FROM sage.query_store q WHERE {db:q}
		  ORDER BY q.captured_at, q.queryid`)
	if err != nil {
		t.Fatalf("read samples: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// placementAnswer is everything the query store answers on the fixture.
type placementAnswer struct {
	rows    []string
	windows map[int64]window
}

func readPlacementAnswer(t *testing.T, p *histfixture.Pair, from, to time.Time) placementAnswer {
	t.Helper()
	ctx := context.Background()
	a := placementAnswer{rows: storeSamples(t, p), windows: map[int64]window{}}
	for _, qid := range []int64{11, 22, 33, 44} {
		a.windows[qid] = windowOf(t, ctx, p.Monitored, qid, from, to)
	}
	return a
}

func TestQueryStoreIdenticalInBothPlacements(t *testing.T) {
	p := histfixture.NewPair(t)
	start := time.Now().Add(-30 * time.Minute).Truncate(time.Minute)
	from, to := start.Add(30*time.Second), start.Add(6*time.Minute)
	got := map[histstore.Mode]placementAnswer{}
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		seedPlans(t, p)
		p.Noise(t, start.Add(2*time.Minute), 11, 22, 33)
		recordCycles(t, p, start)
		got[mode] = readPlacementAnswer(t, p, from, to)
	}
	mon, meta := got[histstore.ModeMonitored], got[histstore.ModeMeta]
	if len(mon.rows) == 0 || len(mon.rows) != len(meta.rows) {
		t.Fatalf("samples: monitored %d rows, meta %d rows", len(mon.rows), len(meta.rows))
	}
	for i := range mon.rows {
		if mon.rows[i] != meta.rows[i] {
			t.Fatalf("sample %d differs:\nmonitored %s\nmeta      %s", i, mon.rows[i],
				meta.rows[i])
		}
	}
	if mon.rows[0] != "11 100 200 2 new11" {
		t.Fatalf("plan_hash must be the newest fingerprint from the monitored database's "+
			"explain_cache, got %q", mon.rows[0])
	}
	for qid, w := range mon.windows {
		if meta.windows[qid] != w {
			t.Fatalf("window for %d: monitored %+v, meta %+v", qid, w, meta.windows[qid])
		}
	}
	if _, ok := mon.windows[11].measured(); !ok {
		t.Fatalf("query 11 must be measured, got %+v", mon.windows[11])
	}
}

func TestQueryStoreMetaModeWritesNothingToTheMonitoredDatabase(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMeta)
	seedPlans(t, p)
	if err := recordNow(context.Background(), p.Monitored, []Sample{{QueryID: 22, Calls: 1,
		TotalExecMs: 1, MeanExecMs: 1}}); err != nil {
		t.Fatalf("record: %v", err)
	}
	mon, meta := p.Count(t, "query_store")
	if mon != 0 || meta != 1 {
		t.Fatalf("meta mode wrote %d monitored / %d store rows, want 0 / 1", mon, meta)
	}
	var hash string
	if err := p.Meta.QueryRow(context.Background(), `SELECT plan_hash FROM sage.query_store
		WHERE database_id = $1`, histfixture.DatabaseID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash != "only22" {
		t.Fatalf("plan_hash = %q, want the monitored fingerprint only22", hash)
	}
}

func TestQueryStoreMetaModeFailsLoudlyWhenTheStoreIsDown(t *testing.T) {
	p := histfixture.NewPair(t)
	closed := histfixture.NewPair(t)
	store, err := histstore.NewMeta(closed.Meta, histfixture.DatabaseID)
	if err != nil {
		t.Fatal(err)
	}
	closed.Meta.Close()
	unreg := histstore.Register(p.Monitored, "app", store)
	defer unreg()
	err = recordNow(context.Background(), p.Monitored, []Sample{{QueryID: 1, Calls: 1,
		TotalExecMs: 1, MeanExecMs: 1}})
	if err == nil {
		t.Fatal("a write to a closed store must fail, never fall back to the monitored DB")
	}
	if mon, _ := p.Count(t, "query_store"); mon != 0 {
		t.Fatalf("the failed write landed %d rows in the monitored database", mon)
	}
	if _, err := QueryIntervals(context.Background(), p.Monitored, 1,
		time.Now().Add(-time.Hour), time.Now(), time.Minute); err == nil {
		t.Fatal("intervals from a closed store must be an error, not 'not sampled'")
	}
}
