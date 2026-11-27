package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
)

// observability-04: executor outcomes, verification verdicts, pending
// approvals and the last failure on /metrics.

// actionFamilies are the four families this item adds.
var actionFamilies = []string{"pg_sage_actions_total", "pg_sage_verification_verdicts_total",
	"pg_sage_actions_pending_approval", "pg_sage_action_last_failure_timestamp_seconds"}

func fakeActionsSource(g executor.ActionGauges, err error) metricsSource {
	return metricsSource{name: metricsSourceActions,
		read: func(_ context.Context, s *metricsSnapshot) error {
			if err != nil {
				return err
			}
			s.actions = g
			return nil
		}}
}

// Nil/empty: with nothing recorded every family is declared, no samples.
func TestWriteActionMetrics_EmptyDeclaresFamiliesOnly(t *testing.T) {
	var b strings.Builder
	writeActionMetrics(&b, nil, nil, nil)
	out := b.String()
	for _, f := range actionFamilies {
		if !strings.Contains(out, "# HELP "+f+" ") || !strings.Contains(out, "# TYPE "+f+" ") {
			t.Fatalf("family %s is not declared:\n%s", f, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			t.Fatalf("empty input rendered a sample %q", line)
		}
	}
	e := lintScrape(t, out)
	if e.kinds["pg_sage_actions_total"] != "counter" ||
		e.kinds["pg_sage_verification_verdicts_total"] != "counter" ||
		e.kinds["pg_sage_actions_pending_approval"] != "gauge" ||
		e.kinds["pg_sage_action_last_failure_timestamp_seconds"] != "gauge" {
		t.Fatalf("family types = %v", e.kinds)
	}
}

func TestWriteActionMetrics_RendersCountsAndGauges(t *testing.T) {
	failed := time.Unix(1_800_000_000, 0)
	views := []metricsView{
		{name: "app", sources: []string{metricsSourceActions}, snap: &metricsSnapshot{
			ok: map[string]bool{metricsSourceActions: true},
			actions: executor.ActionGauges{LastFailure: failed,
				PendingByRisk: map[string]int64{"high": 2, "safe": 0}}}},
		{name: "quiet", sources: []string{metricsSourceActions}, snap: &metricsSnapshot{
			ok:      map[string]bool{metricsSourceActions: true},
			actions: executor.ActionGauges{PendingByRisk: map[string]int64{}}}},
	}
	var b strings.Builder
	writeActionMetrics(&b,
		[]executor.OutcomeCount{{Database: "app", Class: "index", Outcome: "failed", Count: 3}},
		[]executor.VerdictCount{{Database: "app", Class: "index", Verdict: "revert", Count: 1}},
		views)
	out := b.String()
	for _, want := range []string{
		`pg_sage_actions_total{database="app",class="index",outcome="failed"} 3`,
		`pg_sage_verification_verdicts_total{database="app",class="index",verdict="revert"} 1`,
		`pg_sage_actions_pending_approval{database="app",risk="high"} 2`,
		`pg_sage_actions_pending_approval{database="app",risk="safe"} 0`,
		`pg_sage_action_last_failure_timestamp_seconds{database="app"} 1800000000`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Fatalf("metrics lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `last_failure_timestamp_seconds{database="quiet"}`) {
		t.Fatalf("a database that never failed exports a failure time:\n%s", out)
	}
	lintScrape(t, out)
}

// A failed or missing refresh exports no gauge samples: unknown is not 0.
func TestWriteActionMetrics_UnrefreshedGaugesAreOmitted(t *testing.T) {
	views := []metricsView{
		{name: "down", sources: []string{metricsSourceActions}, snap: &metricsSnapshot{
			ok: map[string]bool{metricsSourceActions: false}}},
		{name: "new", sources: []string{metricsSourceActions}},
	}
	var b strings.Builder
	writeActionMetrics(&b, nil, nil, views)
	if strings.Contains(b.String(), `database="down"`) ||
		strings.Contains(b.String(), `database="new"`) {
		t.Fatalf("unrefreshed databases export gauges:\n%s", b.String())
	}
}

// Integration: the full scrape parses with expfmt and carries the four
// families, with the counted rolled-back index build (CHECK-22).
func TestActionMetrics_FullScrapeCarriesTheFamilies(t *testing.T) {
	installHealthFleet(t, "fleet")
	withMetricsRegistry(t)
	db := "obs04-scrape"
	t.Cleanup(func() { executor.PruneCounts(db) })
	executor.RecordOutcome(db, "index", "success")
	executor.RecordOutcome(db, "index", "rolled_back")
	executor.RecordVerdict(db, "index", "revert")
	m := newMetricsRefresher(db, fixedInterval(time.Hour), fakeActionsSource(
		executor.ActionGauges{PendingByRisk: map[string]int64{"moderate": 4}}, nil))
	processMetrics.register(m)
	t.Cleanup(func() { processMetrics.unregister(m) })
	m.refresh(context.Background())
	e := lintScrape(t, renderMetricsBody())
	for _, f := range actionFamilies {
		if _, ok := e.families[f]; !ok && e.kinds[f] == "" {
			t.Fatalf("scrape lacks %s", f)
		}
	}
	for _, want := range []string{
		`pg_sage_actions_total{database="obs04-scrape",class="index",outcome="rolled_back"} 1`,
		`pg_sage_actions_total{database="obs04-scrape",class="index",outcome="success"} 1`,
		`pg_sage_verification_verdicts_total{database="obs04-scrape",class="index",` +
			`verdict="revert"} 1`,
		`pg_sage_actions_pending_approval{database="obs04-scrape",risk="moderate"} 4`,
	} {
		if !strings.Contains(e.body, want+"\n") {
			t.Fatalf("scrape lacks %q:\n%s", want, grepLines(e.body, "obs04"))
		}
	}
	if got := e.labelValues("pg_sage_actions_total", "outcome"); len(got) < 2 {
		t.Fatalf("parsed outcome labels = %v", got)
	}
}

// A failing actions source is reported by the refresh series, and its
// gauges are dropped rather than zeroed.
func TestActionMetrics_FailedSourceIsReported(t *testing.T) {
	installHealthFleet(t, "fleet")
	withMetricsRegistry(t)
	m := newMetricsRefresher("obs04-down", fixedInterval(time.Hour),
		fakeActionsSource(executor.ActionGauges{}, errors.New("relation missing")))
	processMetrics.register(m)
	t.Cleanup(func() { processMetrics.unregister(m) })
	m.refresh(context.Background())
	body := renderMetricsBody()
	want := `pg_sage_metrics_refresh_success{database="obs04-down",source="actions"} 0`
	if !strings.Contains(body, want+"\n") {
		t.Fatalf("scrape lacks %q:\n%s", want, grepLines(body, "obs04-down"))
	}
	if strings.Contains(body, `pg_sage_actions_pending_approval{database="obs04-down"`) {
		t.Fatal("a failed refresh exported pending approvals")
	}
}

// The runtime refresher reads the action gauges in every mode.
func TestRuntimeMetricsRefresher_ReadsActionGauges(t *testing.T) {
	rt := &databaseRuntime{spec: databaseRuntimeSpec{Name: "obs04-rt"},
		cfg: config.DefaultConfig()}
	m := newRuntimeMetricsRefresher(rt)
	found := false
	for _, s := range m.sources {
		found = found || s.name == metricsSourceActions
	}
	if !found {
		t.Fatalf("runtime sources lack %s", metricsSourceActions)
	}
}
