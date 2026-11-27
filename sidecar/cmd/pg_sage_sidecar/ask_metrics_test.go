package main

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/ask"
)

// mcp-07: pg_sage_ask_answers_total{database,status,stop} counts Ask Sage
// answers, so an operator sees how many questions the wall clock or a
// cancel cut short (the trigger for asynchronous answers, mcp-07b).
// No concurrent access tests: the writer formats a snapshot it is given.

func TestWriteAskMetrics_SeriesAndHeader(t *testing.T) {
	var b strings.Builder
	writeAskMetrics(&b, []ask.AnswerCount{
		{Database: "prod", Status: "answered", Stop: "final", Count: 7},
		{Database: "prod", Status: "incomplete", Stop: "wall_clock", Count: 2},
		{Database: "prod", Status: "incomplete", Stop: "cancelled", Count: 1},
	})
	out := b.String()
	for _, want := range []string{
		"# TYPE pg_sage_ask_answers_total counter\n",
		`pg_sage_ask_answers_total{database="prod",status="answered",stop="final"} 7` + "\n",
		`pg_sage_ask_answers_total{database="prod",status="incomplete",stop="wall_clock"} 2`,
		`pg_sage_ask_answers_total{database="prod",status="incomplete",stop="cancelled"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !strings.HasPrefix(out, "# HELP pg_sage_ask_answers_total ") {
		t.Errorf("output does not start with the HELP line:\n%s", out)
	}
}

func TestWriteAskMetrics_EmptyStillDeclaresTheCounter(t *testing.T) {
	var b strings.Builder
	writeAskMetrics(&b, nil)
	out := b.String()
	if !strings.Contains(out, "# TYPE pg_sage_ask_answers_total counter") ||
		strings.Contains(out, "pg_sage_ask_answers_total{") {
		t.Fatalf("empty output = %q", out)
	}
}

func TestMetricsEndpointExposesAskAnswers(t *testing.T) {
	// Fleet mode with no databases: the scrape needs no PostgreSQL.
	preserveValueGlobals(t)
	installValueFleet(map[string]*pgxpool.Pool{})
	if out := scrapeMetrics(t); !strings.Contains(out,
		"# TYPE pg_sage_ask_answers_total counter") {
		t.Fatal("/metrics does not expose pg_sage_ask_answers_total")
	}
}
