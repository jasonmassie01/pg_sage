package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auditchain"
	"github.com/pg-sage/sidecar/internal/auditjob"
	"github.com/pg-sage/sidecar/internal/leader"
	"github.com/pg-sage/sidecar/internal/pgaudit"
	"github.com/pg-sage/sidecar/internal/siem"
)

// audit-12: audit, SIEM and pgaudit health on /metrics.

var auditT0 = time.Unix(1_800_000_000, 0).UTC()

// auditFamilies are the families this item adds; each must render its
// HELP and TYPE even with no samples.
var auditFamilies = map[string]string{
	"pg_sage_audit_chain_ok":                         "gauge",
	"pg_sage_audit_chain_verified_timestamp_seconds": "gauge",
	"pg_sage_audit_chain_problems":                   "gauge",
	"pg_sage_audit_chain_verify_errors_total":        "counter",
	"pg_sage_siem_lag_links":                         "gauge",
	"pg_sage_siem_consecutive_failures":              "gauge",
	"pg_sage_siem_delivered_total":                   "counter",
}

func auditHealthFixture() auditjob.HealthSnapshot {
	h := auditjob.NewHealthRegistry()
	h.Record("orders", []auditchain.Report{
		{Chain: "action_log", HeadSeq: 9, VerifiedAt: auditT0,
			Problems: []auditchain.Problem{{Kind: auditchain.KindRowEdited, Seq: 3},
				{Kind: auditchain.KindRowEdited, Seq: 4}}},
		{Chain: "auth_audit", HeadSeq: 2, VerifiedAt: auditT0},
	}, errors.New("config_audit: timeout"), auditT0)
	h.Record("billing", []auditchain.Report{{Chain: "action_log", HeadSeq: 1,
		VerifiedAt: auditT0.Add(time.Minute)}}, nil, auditT0)
	return h.Snapshot()
}

func sinkFixture() []siem.SinkStatus {
	return []siem.SinkStatus{
		{Name: "soc", Delivered: 40, ConsecutiveFailures: 2, Lanes: []siem.LaneStatus{
			{Source: "orders", Chain: "action_log", Cursor: 5, Head: 9, Lag: 4},
			{Source: "orders", Chain: "auth_audit", Cursor: 2, Head: 2, Lag: 0},
			{Source: "dead", Chain: "", Lag: 0, State: siem.LaneFailing},
		}},
		{Name: "archive", Delivered: 7, Lanes: []siem.LaneStatus{
			{Source: "billing", Chain: "action_log", Cursor: 1, Head: 1}}},
	}
}

func renderAudit(h auditjob.HealthSnapshot, sinks []siem.SinkStatus) string {
	var b strings.Builder
	writeAuditMetrics(&b, h, sinks)
	return b.String()
}

func sampleValue(t *testing.T, e exposition, family string, labels map[string]string) float64 {
	t.Helper()
	f := e.families[family]
	for _, m := range f.GetMetric() {
		got := map[string]string{}
		for _, l := range m.GetLabel() {
			got[l.GetName()] = l.GetValue()
		}
		match := len(got) == len(labels)
		for k, v := range labels {
			match = match && got[k] == v
		}
		if !match {
			continue
		}
		if m.GetCounter() != nil {
			return m.GetCounter().GetValue()
		}
		return m.GetGauge().GetValue()
	}
	t.Fatalf("no %s sample with %v in:\n%s", family, labels, e.body)
	return 0
}

// Happy path: every chain's state, time, problems and error counter, and
// every sink's lag per lane, failures and deliveries, parse as the text
// format with the types promised.
func TestAuditMetricsRenderHealthAndSinks(t *testing.T) {
	e := lintScrape(t, renderAudit(auditHealthFixture(), sinkFixture()))
	for name, kind := range auditFamilies {
		if e.kinds[name] != kind {
			t.Errorf("%s TYPE = %q, want %s", name, e.kinds[name], kind)
		}
	}
	ok := func(db, chain string) float64 {
		return sampleValue(t, e, "pg_sage_audit_chain_ok",
			map[string]string{"database": db, "chain": chain})
	}
	if ok("orders", "action_log") != 0 || ok("orders", "auth_audit") != 1 ||
		ok("billing", "action_log") != 1 {
		t.Fatalf("chain_ok wrong:\n%s", e.body)
	}
	if v := sampleValue(t, e, "pg_sage_audit_chain_verified_timestamp_seconds",
		map[string]string{"database": "billing", "chain": "action_log"}); v != float64(
		auditT0.Add(time.Minute).Unix()) {
		t.Fatalf("verified timestamp = %v", v)
	}
	if v := sampleValue(t, e, "pg_sage_audit_chain_problems", map[string]string{
		"database": "orders", "chain": "action_log", "kind": "row_edited"}); v != 2 {
		t.Fatalf("row_edited problems = %v, want 2", v)
	}
	if n := len(e.families["pg_sage_audit_chain_problems"].GetMetric()); n != 1 {
		t.Fatalf("%d problem series, want 1 (a clean chain has none)", n)
	}
	if v := sampleValue(t, e, "pg_sage_audit_chain_verify_errors_total",
		map[string]string{"database": "orders"}); v != 1 {
		t.Fatalf("orders verify errors = %v, want 1", v)
	}
	if v := sampleValue(t, e, "pg_sage_audit_chain_verify_errors_total",
		map[string]string{"database": "billing"}); v != 0 {
		t.Fatalf("billing verify errors = %v, want an explicit 0", v)
	}
	if v := sampleValue(t, e, "pg_sage_siem_lag_links", map[string]string{
		"sink": "soc", "source": "orders", "chain": "action_log"}); v != 4 {
		t.Fatalf("soc lag = %v, want 4", v)
	}
	if v := sampleValue(t, e, "pg_sage_siem_consecutive_failures",
		map[string]string{"sink": "soc"}); v != 2 {
		t.Fatalf("soc failures = %v", v)
	}
	if v := sampleValue(t, e, "pg_sage_siem_delivered_total",
		map[string]string{"sink": "archive"}); v != 7 {
		t.Fatalf("archive delivered = %v", v)
	}
}

// CHECK-27: label cardinality is databases x chains for the chain
// families and sinks x sources x chains for lag; a source lane (no
// chain) is no lag series.
func TestAuditMetricsCardinality(t *testing.T) {
	e := lintScrape(t, renderAudit(auditHealthFixture(), sinkFixture()))
	for _, f := range []string{"pg_sage_audit_chain_ok",
		"pg_sage_audit_chain_verified_timestamp_seconds"} {
		if n := len(e.families[f].GetMetric()); n != 3 {
			t.Errorf("%s has %d series, want 3 (databases x chains)", f, n)
		}
	}
	if n := len(e.families["pg_sage_siem_lag_links"].GetMetric()); n != 3 {
		t.Errorf("lag has %d series, want 3 (sink lanes with a chain)", n)
	}
	for _, f := range []string{"pg_sage_siem_consecutive_failures",
		"pg_sage_siem_delivered_total"} {
		if n := len(e.families[f].GetMetric()); n != 2 {
			t.Errorf("%s has %d series, want 2 (sinks)", f, n)
		}
	}
	if strings.Contains(e.body, `source="dead"`) {
		t.Errorf("a source lane without a chain became a lag series:\n%s", e.body)
	}
}

// Nil/empty: no verification yet and no sinks emit HELP and TYPE only.
func TestAuditMetricsEmptyEmitsHelpAndTypeOnly(t *testing.T) {
	body := renderAudit(auditjob.HealthSnapshot{}, nil)
	e := lintScrape(t, body)
	if len(e.order) != 0 {
		t.Fatalf("empty registry rendered samples: %q", e.order)
	}
	for name, kind := range auditFamilies {
		if !strings.Contains(body, "# HELP "+name+" ") ||
			!strings.Contains(body, "# TYPE "+name+" "+kind+"\n") {
			t.Errorf("empty render lacks HELP/TYPE of %s", name)
		}
	}
}

// Invalid input: hostile database, chain and sink names stay parseable
// (observability-07), and no problem detail or row content reaches a
// label.
func TestAuditMetricsHostileNames(t *testing.T) {
	h := auditjob.NewHealthRegistry()
	h.Record("a\tb\"c", []auditchain.Report{{Chain: "action_log", VerifiedAt: auditT0,
		Problems: []auditchain.Problem{{Kind: "gap", Detail: "secret row content"}}}}, nil,
		auditT0)
	sinks := []siem.SinkStatus{{Name: "s\nink", Lanes: []siem.LaneStatus{
		{Source: "a\tb\"c", Chain: "action_log", Lag: 1}}}}
	e := lintScrape(t, renderAudit(h.Snapshot(), sinks))
	if v := sampleValue(t, e, "pg_sage_audit_chain_ok",
		map[string]string{"database": "a\tb\"c", "chain": "action_log"}); v != 0 {
		t.Fatalf("hostile database chain_ok = %v", v)
	}
	if strings.Contains(e.body, "secret row content") {
		t.Fatalf("problem detail leaked into /metrics:\n%s", e.body)
	}
}

// Invalid input: pgaudit's record counter uses the shared label escaping
// too; a database name with a tab broke the scrape with Go's %q.
func TestPGAuditMetricsHostileDatabaseParses(t *testing.T) {
	var b strings.Builder
	writePGAuditMetrics(&b, []pgaudit.RecordCount{{Database: "a\tb", Outcome: "stored",
		Count: 3}})
	e := lintScrape(t, b.String())
	if v := sampleValue(t, e, "pg_sage_pgaudit_records_total",
		map[string]string{"database": "a\tb", "outcome": "stored"}); v != 3 {
		t.Fatalf("pgaudit stored = %v", v)
	}
}

// The scrape carries the audit families from the process registry and
// the running exporter.
func TestHandleMetricsIncludesAuditFamilies(t *testing.T) {
	installContractFleet(t, "fleet", "alpha")
	resetAuditHealth(t)
	auditjob.DefaultHealth().Record("alpha", []auditchain.Report{{Chain: "action_log",
		VerifiedAt: auditT0}}, nil, auditT0)
	e := lintScrape(t, scrapeMetrics(t))
	if v := sampleValue(t, e, "pg_sage_audit_chain_ok",
		map[string]string{"database": "alpha", "chain": "action_log"}); v != 1 {
		t.Fatalf("scrape chain_ok = %v", v)
	}
	for name := range auditFamilies {
		if e.kinds[name] == "" {
			t.Errorf("scrape lacks %s", name)
		}
	}
}

func resetAuditHealth(t *testing.T) {
	t.Helper()
	auditjob.DefaultHealth().Reset()
	t.Cleanup(auditjob.DefaultHealth().Reset)
}

func closedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), "postgres://sage@127.0.0.1:1/none")
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	p.Close()
	return p
}

// Error propagation: a verification that errors (closed pool) keeps the
// previous result and counts the error on the scrape.
func TestRunChainVerificationErrorKeepsPreviousResult(t *testing.T) {
	preserveAuditFenceGlobals(t)
	setFleetLeader(nil)
	resetAuditHealth(t)
	h := auditjob.DefaultHealth()
	h.Record("orders", []auditchain.Report{{Chain: "action_log", VerifiedAt: auditT0}}, nil,
		auditT0)
	runChainVerification(context.Background(), []auditjob.Source{{Name: "orders",
		Pool: closedPool(t)}})
	e := lintScrape(t, renderAudit(h.Snapshot(), nil))
	if v := sampleValue(t, e, "pg_sage_audit_chain_ok",
		map[string]string{"database": "orders", "chain": "action_log"}); v != 1 {
		t.Fatalf("chain_ok after a failed run = %v, want the previous 1", v)
	}
	if v := sampleValue(t, e, "pg_sage_audit_chain_verify_errors_total",
		map[string]string{"database": "orders"}); v != 1 {
		t.Fatalf("verify errors = %v, want 1", v)
	}
}

// State transitions: a database no longer among the sources loses its
// series; a sidecar that is not the leader exports no chain series at all.
func TestRunChainVerificationRetainsSourcesAndFollowerExportsNothing(t *testing.T) {
	preserveAuditFenceGlobals(t)
	setFleetLeader(nil)
	resetAuditHealth(t)
	h := auditjob.DefaultHealth()
	for _, db := range []string{"orders", "gone"} {
		h.Record(db, []auditchain.Report{{Chain: "action_log", VerifiedAt: auditT0}}, nil,
			auditT0)
	}
	runChainVerification(context.Background(), []auditjob.Source{{Name: "orders",
		Pool: closedPool(t)}})
	for _, c := range h.Snapshot().Chains {
		if c.Database == "gone" {
			t.Fatalf("a removed database kept its chain series: %+v", c)
		}
	}
	setFleetLeader(leader.NewElector(stubLease{held: false}, "s", "me", time.Minute))
	_ = currentFleetLeader().Tick(context.Background())
	runChainVerification(context.Background(), []auditjob.Source{{Name: "orders",
		Pool: closedPool(t)}})
	if s := h.Snapshot(); len(s.Chains) != 0 || len(s.Errors) != 0 {
		t.Fatalf("follower still exports chain health: %+v", s)
	}
}

// siemSinkStatuses reads the running exporter; none without one.
func TestSIEMSinkStatusesFollowTheExporter(t *testing.T) {
	old := auditExport.Load()
	t.Cleanup(func() { auditExport.Store(old) })
	auditExport.Store(nil)
	if got := siemSinkStatuses(); got != nil {
		t.Fatalf("no exporter: statuses = %v, want nil", got)
	}
	auditExport.Store(siem.NewExporter(func() []siem.Source { return nil },
		[]siem.SinkConfig{{Sink: siem.NewHTTPSink("soc", "https://x.invalid", "", time.Second,
			nil)}}, nil, siem.Options{}, func() (siem.Fence, bool) { return siem.Fence{}, false },
		nil))
	got := siemSinkStatuses()
	if len(got) != 1 || got[0].Name != "soc" || got[0].Delivered != 0 {
		t.Fatalf("statuses = %+v, want soc with nothing delivered", got)
	}
}

// Concurrent access: verification records, the registry renders and
// snapshots are taken at once; every scrape parses.
func TestAuditMetricsConcurrentRecordAndRender(t *testing.T) {
	h := auditjob.NewHealthRegistry()
	var wg sync.WaitGroup
	for w := 0; w < 3; w++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				h.Record("orders", []auditchain.Report{{Chain: "action_log",
					HeadSeq: int64(i), VerifiedAt: auditT0}}, nil, auditT0)
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				body := renderAudit(h.Snapshot(), sinkFixture())
				if !strings.Contains(body, "# TYPE pg_sage_audit_chain_ok gauge") {
					t.Errorf("render %d lacks chain_ok", i)
					return
				}
			}
		}()
	}
	wg.Wait()
	lintScrape(t, renderAudit(h.Snapshot(), sinkFixture()))
}
