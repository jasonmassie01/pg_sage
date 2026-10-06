package querystore

import (
	"context"
	"fmt"
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

func TestQueryStoreIdenticalInBothPlacements(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	start := time.Now().Add(-30 * time.Minute).Truncate(time.Minute)
	from, to := start.Add(30*time.Second), start.Add(6*time.Minute)
	type answer struct {
		rows      []string
		evidence  map[int64]Evidence
		latencies string
	}
	got := map[histstore.Mode]answer{}
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		seedPlans(t, p)
		p.Noise(t, start.Add(2*time.Minute), 11, 22, 33)
		recordCycles(t, p, start)
		a := answer{rows: storeSamples(t, p), evidence: map[int64]Evidence{}}
		for _, qid := range []int64{11, 22, 33, 44} {
			ev, err := WindowedLatencyEvidence(ctx, p.Monitored, qid, from, to)
			if err != nil {
				t.Fatalf("%s evidence %d: %v", mode, qid, err)
			}
			a.evidence[qid] = ev
			ms, ok, err := WindowedLatencyMs(ctx, p.Monitored, qid, from)
			if err != nil {
				t.Fatalf("%s latency %d: %v", mode, qid, err)
			}
			a.latencies += fmt.Sprintf("%d:%.4f:%v ", qid, ms, ok)
		}
		got[mode] = a
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
	for qid, ev := range mon.evidence {
		if meta.evidence[qid] != ev {
			t.Fatalf("evidence for %d: monitored %+v, meta %+v", qid, ev, meta.evidence[qid])
		}
	}
	if mon.evidence[11].Status != EvidenceMeasured {
		t.Fatalf("query 11 must be measured, got %+v", mon.evidence[11])
	}
	if mon.latencies != meta.latencies {
		t.Fatalf("latencies: monitored %s, meta %s", mon.latencies, meta.latencies)
	}
}

func TestQueryStoreMetaModeWritesNothingToTheMonitoredDatabase(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMeta)
	seedPlans(t, p)
	if err := Record(context.Background(), p.Monitored, []Sample{{QueryID: 22, Calls: 1,
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
	err = Record(context.Background(), p.Monitored, []Sample{{QueryID: 1, Calls: 1,
		TotalExecMs: 1, MeanExecMs: 1}})
	if err == nil {
		t.Fatal("a write to a closed store must fail, never fall back to the monitored DB")
	}
	if mon, _ := p.Count(t, "query_store"); mon != 0 {
		t.Fatalf("the failed write landed %d rows in the monitored database", mon)
	}
	if _, err := WindowedLatencyEvidence(context.Background(), p.Monitored, 1,
		time.Now().Add(-time.Hour), time.Now()); err == nil {
		t.Fatal("evidence from a closed store must be an error, not 'not sampled'")
	}
}
