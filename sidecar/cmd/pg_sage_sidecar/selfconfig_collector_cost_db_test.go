package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/selfconfig"
)

// footprint-05, integration (real Postgres): the ledger state cites the
// source of the cost evidence. The startup pass (before the collector ran)
// cites the probe; a live pass after a metered cycle cites the collector's
// slowest statement, as GET /api/v1/derived-settings serves it.

func citedIn(t *testing.T, r *selfConfigRunner, key, name string) selfconfig.Citation {
	t.Helper()
	st, err := r.engine.Store.Get(t.Context(), key)
	if err != nil {
		t.Fatalf("state of %s: %v", key, err)
	}
	for _, c := range st.Evidence {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("%s cites %+v, want %s", key, st.Evidence, name)
	return selfconfig.Citation{}
}

func TestSelfConfigLedgerCitesTheCostSource(t *testing.T) {
	pool := selfConfigPool(t)
	cfg := config.DefaultConfig()
	r := newTestRunner(pool, cfg, &logCapture{})
	if _, err := r.pass(t.Context(), selfconfig.PhaseStartup); err != nil {
		t.Fatal(err)
	}
	if c := citedIn(t, r, "safety.query_timeout_ms", "catalog_scan_ms"); c.Source !=
		selfconfig.SourceProbe {
		t.Fatalf("startup citation %+v, want source probe", c)
	}
	r.collectorCost = func() (selfconfig.CollectorCost, bool) {
		return selfconfig.CollectorCost{Cycles: 10, CycleMs: 2400, SlowestMs: 700,
			SlowestStep: "indexes", Timeouts: 1}, true
	}
	results, err := r.pass(t.Context(), selfconfig.PhaseLive)
	if err != nil {
		t.Fatal(err)
	}
	got := byKey(results)["safety.query_timeout_ms"]
	// 3x 700 ms = 2100; one timeout raises to 2x the current 500: 2100 wins.
	if got.Shadow == nil || *got.Shadow != 2100 {
		t.Fatalf("query timeout %+v, want a 2100 ms shadow", got)
	}
	c := citedIn(t, r, "safety.query_timeout_ms", "slowest_catalog_ms")
	if c.Source != selfconfig.SourceCollector || c.Value != 700 || c.Detail != "indexes" {
		t.Fatalf("live citation %+v, want 700 ms from the collector naming indexes", c)
	}
	seen := citedIn(t, r, "safety.query_timeout_ms", "timeouts_seen")
	raw, err := json.Marshal(seen)
	if err != nil || !strings.Contains(string(raw), `"source":"collector"`) {
		t.Fatalf("timeouts_seen serialised as %s (%v)", raw, err)
	}
}
