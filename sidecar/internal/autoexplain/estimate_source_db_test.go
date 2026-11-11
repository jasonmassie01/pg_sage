package autoexplain

import (
	"context"
	"testing"
)

// rca-04: the on-demand capture is a plain EXPLAIN (no ANALYZE), so it is
// stored as explain_estimate, never as auto_explain, and it sends no
// auto_explain settings: auto_explain never logs an EXPLAIN statement, so
// a LOAD or SET LOCAL per parameter (each under a savepoint) did nothing.

func (f *lockFixture) collectorWith(avail *Availability) *Collector {
	c := f.collector()
	c.avail = avail
	return c
}

func TestCaptureOnDemandStoresAnEstimateWithoutAutoExplainSettings(t *testing.T) {
	for _, avail := range []*Availability{
		{SharedPreload: true, Available: true, Method: "shared_preload"},
		{SessionLoad: true, Available: true, Method: "session_load"},
		{AlreadyLoaded: true, Available: true, Method: "already_loaded"},
		nil,
	} {
		f := newLockFixture(t)
		const qid = int64(991200511)
		f.cleanPlans(t, qid)
		err := f.collectorWith(avail).captureOnDemand(context.Background(), qid,
			"SELECT * FROM "+lockTable+" WHERE id = 1")
		if err != nil {
			t.Fatalf("capture (%+v): %v", avail, err)
		}
		if got := f.storedPlans(t, qid); got[qid] != estimateSource {
			t.Fatalf("stored plans (%+v) = %v, want source %s", avail, got, estimateSource)
		}
		for _, fragment := range []string{"auto_explain", "SAVEPOINT", "LOAD"} {
			if sent := f.rec.Matching(fragment); len(sent) != 0 {
				t.Fatalf("capture (%+v) sent %q statements: %v", avail, fragment, sent)
			}
		}
		var execTime float64
		if err := f.pool.QueryRow(context.Background(), `SELECT execution_time
			FROM sage.explain_cache WHERE queryid = $1`, qid).Scan(&execTime); err != nil {
			t.Fatal(err)
		}
		if execTime != 0 {
			t.Fatalf("an estimate has no execution time, stored %v", execTime)
		}
		f.assertBoundsRolledBack(t)
	}
}

func TestEstimateSourceIsNotAnAutoExplainLabel(t *testing.T) {
	if estimateSource != "explain_estimate" {
		t.Fatalf("estimateSource = %q, want explain_estimate", estimateSource)
	}
}
