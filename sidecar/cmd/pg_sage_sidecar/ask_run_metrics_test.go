package main

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/ask"
)

// mcp-07b: pg_sage_ask_runs_active{database} and
// pg_sage_ask_runs_total{database,state}. No concurrent access tests:
// the writer formats snapshots it is given.

func TestWriteAskRunMetrics_SeriesAndHeaders(t *testing.T) {
	var b strings.Builder
	writeAskRunMetrics(&b, []ask.ActiveRun{{Database: "prod", Active: 3}},
		[]ask.RunCount{{Database: "prod", State: "done", Count: 9},
			{Database: "prod", State: "interrupted", Count: 1}})
	out := b.String()
	for _, want := range []string{
		"# TYPE pg_sage_ask_runs_active gauge\n",
		`pg_sage_ask_runs_active{database="prod"} 3` + "\n",
		"# TYPE pg_sage_ask_runs_total counter\n",
		`pg_sage_ask_runs_total{database="prod",state="done"} 9` + "\n",
		`pg_sage_ask_runs_total{database="prod",state="interrupted"} 1` + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestWriteAskRunMetrics_EmptyStillDeclaresBoth(t *testing.T) {
	var b strings.Builder
	writeAskRunMetrics(&b, nil, nil)
	out := b.String()
	if !strings.Contains(out, "# TYPE pg_sage_ask_runs_active gauge") ||
		!strings.Contains(out, "# TYPE pg_sage_ask_runs_total counter") ||
		strings.Contains(out, "{database=") {
		t.Fatalf("empty output = %q", out)
	}
}
