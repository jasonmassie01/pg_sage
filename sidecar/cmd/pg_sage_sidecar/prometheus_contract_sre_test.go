package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/selfbudget"
)

// observability-06 CHECK-18: the exposition contract parses a scrape that
// carries the SRE backlog and the loop error and last-success families.

// No concurrent access tests here: scrape concurrency is covered by
// TestRefresherConcurrentScrapesSeeOneSnapshot.

func TestContractScrapeWithSREBacklogAndLoopHealth(t *testing.T) {
	installContractFleet(t, "fleet", "alpha", "beta")
	read := func(context.Context) (sreBacklog, error) {
		return sreBacklog{byState: map[string]int64{"queued": 2, "concluded": 1},
			oldestOpen: time.Now().Add(-time.Minute)}, nil
	}
	m := newMetricsRefresher("alpha", fixedInterval(time.Hour), sreMetricsSource(read))
	processMetrics.register(m)
	t.Cleanup(func() { processMetrics.unregister(m) })
	m.refresh(context.Background())
	selfbudget.Process().TrackErr("sre")(errors.New("bind failed"))
	selfbudget.Process().TrackErr("sre")(nil)

	e := lintScrape(t, scrapeMetrics(t))
	for name, kind := range map[string]string{
		"pg_sage_sre_investigations":                        "gauge",
		"pg_sage_sre_investigation_oldest_open_age_seconds": "gauge",
		"pg_sage_self_loop_errors_total":                    "counter",
		"pg_sage_self_loop_last_success_timestamp_seconds":  "gauge",
		"pg_sage_self_loop_busy_seconds_total":              "counter",
		"pg_sage_self_loop_runs_total":                      "counter",
	} {
		if e.kinds[name] != kind {
			t.Errorf("%s TYPE = %q, want %s", name, e.kinds[name], kind)
		}
	}
	queued := false
	for _, mf := range e.families["pg_sage_sre_investigations"].GetMetric() {
		labels := map[string]string{}
		for _, l := range mf.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		if labels["database"] == "alpha" && labels["state"] == "queued" {
			queued = mf.GetGauge().GetValue() == 2
		}
	}
	if !queued {
		t.Errorf("no alpha queued=2 sample:\n%s", grepLines(e.body, "sre_investigations"))
	}
	if got := e.labelValues("pg_sage_sre_investigation_oldest_open_age_seconds",
		"database"); len(got) != 1 || got[0] != "alpha" {
		t.Errorf("oldest open age databases = %q, want alpha only", got)
	}
}
