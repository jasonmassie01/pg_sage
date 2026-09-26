package querystore

import (
	"math"
	"testing"
	"time"
)

// G1-B05: duplicate queryids in one batch (pg_stat_statements rows split
// by userid/toplevel) must become ONE sample with summed counters, so the
// window endpoints are deterministic.
func TestRecord_AggregatesDuplicateQueryIDs(t *testing.T) {
	pool, ctx := requireDB(t)
	defer pool.Close()
	const qid = int64(505050505)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.query_store WHERE queryid=$1", qid)
	err := Record(ctx, pool, []Sample{
		{QueryID: qid, Calls: 10, TotalExecMs: 100, MeanExecMs: 10, Rows: 1},
		{QueryID: qid, Calls: 20, TotalExecMs: 300, MeanExecMs: 15, Rows: 2},
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	rows, err := pool.Query(ctx, `SELECT calls, total_exec_time, mean_exec_time, rows
		FROM sage.query_store WHERE queryid=$1`, qid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
		var calls, r int64
		var total, mean float64
		if err := rows.Scan(&calls, &total, &mean, &r); err != nil {
			t.Fatal(err)
		}
		if calls != 30 || total != 400 || r != 3 || math.Abs(mean-400.0/30) > 1e-9 {
			t.Errorf("sample = calls %d total %v mean %v rows %d; want 30/400/13.33/3",
				calls, total, mean, r)
		}
	}
	if n != 1 {
		t.Fatalf("stored %d rows for one queryid in one cycle, want 1", n)
	}
}

// G1-B26: verification must distinguish "never sampled" (unknown) from
// measured or insufficient evidence instead of collapsing all into ok=false.
func TestWindowedLatencyEvidence_Statuses(t *testing.T) {
	pool, ctx := requireDB(t)
	defer pool.Close()
	const qid = int64(262626262)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.query_store WHERE queryid=$1", qid)
	from := time.Now().Add(-time.Hour)

	ev, err := WindowedLatencyEvidence(ctx, pool, qid, from, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Status != EvidenceNotSampled {
		t.Fatalf("status = %q, want %q", ev.Status, EvidenceNotSampled)
	}

	record := func(calls int64, total float64) {
		t.Helper()
		if err := Record(ctx, pool, []Sample{
			{QueryID: qid, Calls: calls, TotalExecMs: total}}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	record(100, 1000)
	ev, _ = WindowedLatencyEvidence(ctx, pool, qid, from, time.Now().Add(time.Hour))
	if ev.Status != EvidenceInsufficientSamples {
		t.Fatalf("one sample: status = %q, want %q", ev.Status, EvidenceInsufficientSamples)
	}
	record(200, 4000)
	ev, _ = WindowedLatencyEvidence(ctx, pool, qid, from, time.Now().Add(time.Hour))
	if ev.Status != EvidenceMeasured || math.Abs(ev.LatencyMs-30) > 1e-9 {
		t.Fatalf("measured evidence = %+v, want measured 30ms", ev)
	}
	record(5, 50)
	ev, _ = WindowedLatencyEvidence(ctx, pool, qid, from, time.Now().Add(time.Hour))
	if ev.Status != EvidenceCountersReset {
		t.Fatalf("after reset: status = %q, want %q", ev.Status, EvidenceCountersReset)
	}
}

// G1-B26: an idle query (sampled, but no new calls) is not a regression
// signal and is reported distinctly from "not sampled".
func TestWindowedLatencyEvidence_NoNewCalls(t *testing.T) {
	pool, ctx := requireDB(t)
	defer pool.Close()
	const qid = int64(262626263)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.query_store WHERE queryid=$1", qid)
	from := time.Now().Add(-time.Minute)
	for i := 0; i < 2; i++ {
		if err := Record(ctx, pool, []Sample{
			{QueryID: qid, Calls: 50, TotalExecMs: 500}}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	ev, err := WindowedLatencyEvidence(ctx, pool, qid, from, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Status != EvidenceNoNewCalls {
		t.Fatalf("status = %q, want %q", ev.Status, EvidenceNoNewCalls)
	}
	ms, ok, err := WindowedLatencyMsBetween(ctx, pool, qid, from, time.Now().Add(time.Minute))
	if err != nil || ok || ms != 0 {
		t.Fatalf("legacy wrapper = (%v, %v, %v), want (0, false, nil)", ms, ok, err)
	}
}
