package main

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/querystore"
)

// evid-01a: the rollup's hours by result and its lag render as one series
// each, under their registered headers. No concurrency test: the writer
// renders a snapshot the querystore took under its lock.

func TestWriteQueryRollupMetrics_RendersEverySeries(t *testing.T) {
	var b strings.Builder
	writeQueryRollupMetrics(&b, []querystore.RollupHourCount{
		{Database: "app", Result: "ok", Count: 72},
		{Database: "app", Result: "failed", Count: 1},
	}, []querystore.RollupLag{{Database: "app", Hours: 3}})
	out := b.String()
	for _, want := range []string{
		"# TYPE pg_sage_query_rollup_hours_total counter",
		`pg_sage_query_rollup_hours_total{database="app",result="ok"} 72`,
		`pg_sage_query_rollup_hours_total{database="app",result="failed"} 1`,
		"# TYPE pg_sage_query_rollup_lag_hours gauge",
		`pg_sage_query_rollup_lag_hours{database="app"} 3`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %q:\n%s", want, out)
		}
	}
}

func TestWriteQueryRollupMetrics_NoSeriesBeforeTheFirstPass(t *testing.T) {
	var b strings.Builder
	writeQueryRollupMetrics(&b, nil, nil)
	if strings.Contains(b.String(), "{database=") {
		t.Fatalf("no pass yet must render no series:\n%s", b.String())
	}
}
