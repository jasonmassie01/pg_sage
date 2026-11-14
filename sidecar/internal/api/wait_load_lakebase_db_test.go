package api

import (
	"net/http"
	"testing"

	"github.com/pg-sage/sidecar/internal/lakebaseobs"
)

// market-01b on a live server: with collector samples stored and a fake
// Lakebase seam, the answer carries both series and prefers the counters.
func TestWaitLoadDB_LakebaseSeriesBesideActivitySamples(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedExplorer(t, ctx, pool)
	seam := &fakeWaitSeam{responses: []lakebaseobs.WaitLoadSeries{lakebaseSeries()}}
	mux := seamMux(phase2MgrWithPool(pool), seam.read)
	code, body := getWorkload(t, mux, "/api/v1/wait-load?database=testdb&"+windowQuery())
	if code != http.StatusOK {
		t.Fatalf("status %d (%v), want 200", code, body)
	}
	activity, _ := body["series"].([]any)
	if len(activity) != 2 || body["coverage"] != "full" || body["source"] != waitSourceActivity {
		t.Fatalf("activity series %v coverage %v, want the 2 stored samples in full",
			body["series"], body["coverage"])
	}
	p, _ := body["provider"].(map[string]any)
	lakebase, _ := p["series"].([]any)
	if body["preferred_source"] != "lakebase_wait_counters" || p["source"] != "lakebase" ||
		len(lakebase) != 2 {
		t.Fatalf("body %v, want the 2-point Lakebase series preferred", body)
	}
	if seam.calls != 1 || seam.database != "testdb" || !seam.from.Equal(explorerFrom) {
		t.Fatalf("seam got %d calls for %q from %v, want one read of testdb's window",
			seam.calls, seam.database, seam.from)
	}
}
