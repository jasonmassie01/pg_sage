package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/changefeed"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/sre/slo"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Sage SRE M5 wiring: config becomes SLO objectives (filtered per
// database), burn rules and the Prometheus connector; a page-level burn
// of an app SLI opens an slo_burn investigation, a proxy burn only with
// sre.automatic_start; metrics expose the error-budget state.

func sloSettings() config.SREConfig {
	s := config.DefaultConfig().SRE
	s.SLO.Push.HMACSecret = "0123456789abcdef0123456789abcdef"
	s.SLO.Objectives = []config.SREObjectiveConfig{
		{Name: "checkout", Database: "orders", Source: "push", Target: 0.999},
		{Name: "payments", Source: "push", Target: 0.995, WindowDays: 7,
			MinEligibleEvents: 10, StaleAfterSeconds: 120, Owner: "team-pay"},
	}
	return s
}

func TestSLOObjectivesFromConfig(t *testing.T) {
	s := sloSettings()
	orders, err := sloObjectives(s.SLO, "orders")
	if err != nil || len(orders) != 2 {
		t.Fatalf("orders objectives = %+v err=%v", orders, err)
	}
	billing, err := sloObjectives(s.SLO, "billing")
	if err != nil || len(billing) != 1 || billing[0].Name != "payments" {
		t.Fatalf("billing objectives = %+v err=%v", billing, err)
	}
	p := billing[0]
	if p.Kind != slo.KindApp || p.Source != slo.SourcePush || p.Target != 0.995 ||
		p.Window != 7*24*time.Hour || p.MinEligible != 10 || p.StaleAfter != 2*time.Minute ||
		p.Owner != "team-pay" || p.Database != "billing" || p.Validate() != nil {
		t.Fatalf("payments = %+v", p)
	}
	c := orders[0]
	if c.Window != 30*24*time.Hour || c.MinEligible != 50 || c.StaleAfter != 5*time.Minute {
		t.Fatalf("checkout defaults = %+v", c)
	}
}

func TestSLORulesAndProxyConfig(t *testing.T) {
	s := config.DefaultConfig().SRE
	rules, err := sloRules(s.SLO)
	if err != nil || len(rules) != 3 || rules[0] != slo.DefaultRules()[0] {
		t.Fatalf("default rules = %+v err=%v", rules, err)
	}
	s.SLO.BurnRules = []config.SREBurnRuleConfig{{Severity: "page", LongWindow: "2h",
		ShortWindow: "10m", Factor: 10}}
	rules, err = sloRules(s.SLO)
	if err != nil || len(rules) != 1 || rules[0].Long != 2*time.Hour || rules[0].Factor != 10 {
		t.Fatalf("custom rules = %+v err=%v", rules, err)
	}
	pc := sloProxyConfig(s.SLO.Proxies)
	if pc.Target != 0.99 || pc.Window != 30*24*time.Hour || pc.TopQueries != 20 ||
		pc.ReplicationLagBudget != time.Minute || pc.LatencyFactor != 3 {
		t.Fatalf("proxy config = %+v", pc)
	}
}

func TestPrometheusClientFromConfig(t *testing.T) {
	none, err := prometheusClient(config.SREPrometheusConfig{})
	if err != nil || none != nil {
		t.Fatalf("no url: client=%v err=%v", none, err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := prometheusClient(config.SREPrometheusConfig{URL: "http://prom:9090",
		BearerTokenFile: path, TimeoutSeconds: 3})
	if err != nil || c == nil || c.Timeout() != 3*time.Second {
		t.Fatalf("client = %v err=%v", c, err)
	}
	_, err = prometheusClient(config.SREPrometheusConfig{URL: "http://prom:9090",
		BearerTokenFile: filepath.Join(dir, "missing"), TimeoutSeconds: 3})
	if err == nil {
		t.Fatal("a missing token file was accepted")
	}
}

func TestWriteSLOStatusMetrics(t *testing.T) {
	burn := 16.5
	budget := 0.25
	sts := []slo.Status{
		{Name: "checkout", Kind: slo.KindApp, State: slo.StatePage, FastBurning: true,
			BudgetRemaining: &budget, Rules: []slo.RuleResult{{Severity: slo.SeverityPage,
				Factor: 14.4, Long: slo.WindowView{Window: "1h", BurnRate: &burn},
				Short: slo.WindowView{Window: "5m"}}}},
		{Name: "db_latency", Kind: slo.KindProxy, State: slo.StateUnknown},
	}
	var b strings.Builder
	writeSLOStatusMetrics(&b, map[string][]slo.Status{"orders": sts})
	out := b.String()
	for _, want := range []string{
		`pg_sage_slo_state{database="orders",slo="checkout",kind="app",state="page"} 1`,
		`pg_sage_slo_state{database="orders",slo="checkout",kind="app",state="ok"} 0`,
		`pg_sage_slo_state{database="orders",slo="db_latency",kind="proxy",state="unknown"} 1`,
		`pg_sage_slo_burn_rate{database="orders",slo="checkout",kind="app",window="1h"} 16.5`,
		`pg_sage_slo_error_budget_remaining{database="orders",slo="checkout",kind="app"} 0.25`,
		"# TYPE pg_sage_slo_burn_rate gauge",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %s\n%s", want, out)
		}
	}
	if strings.Contains(out, `window="5m"`) {
		t.Error("an unknown burn rate was exported as a number")
	}
	if strings.Contains(out, `slo="db_latency",kind="proxy"} `) {
		t.Error("an unknown budget was exported as a number")
	}
	var empty strings.Builder
	writeSLOStatusMetrics(&empty, nil)
	if empty.Len() != 0 {
		t.Fatalf("no SLOs wrote %q", empty.String())
	}
}

// signalsFixture builds one database's signals and investigator on the
// live database.
type signalsRig struct {
	sig   *sreSignals
	coord *sre.Coordinator
	store *sre.PostgresStore
	pool  *pgxpool.Pool
	ctx   context.Context
}

func signalsFixture(t *testing.T, settings config.SREConfig) signalsRig {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	sig, err := newSRESignals(sreSignalsDeps{control: pool, monitored: pool, name: "orders",
		settings: settings, logFn: func(string, string, ...any) {}})
	if err != nil {
		t.Fatalf("newSRESignals: %v", err)
	}
	st, err := sre.NewPostgresStore(pool, sre.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	coord, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: st,
		Runner: probes.NewRunner(pool, probes.Catalog(), nil), Signals: sig.probes(),
		Config: sre.DefaultCoordinatorConfig("signals-wiring:" + string(sre.NewUUID()))})
	if err != nil {
		t.Fatal(err)
	}
	sig.attach(coord)
	if _, err := coord.Bind(ctx); err != nil {
		t.Fatal(err)
	}
	return signalsRig{sig: sig, coord: coord, store: st, pool: pool, ctx: ctx}
}

func pageStatus(name string, kind slo.Kind) slo.Status {
	start := time.Now().UTC().Truncate(time.Second)
	return slo.Status{Name: name, Kind: kind, State: slo.StatePage, FastBurning: true,
		BurnStartedAt: &start}
}

func burnInvestigations(t *testing.T, r signalsRig) int {
	t.Helper()
	scope, _ := r.coord.Scope()
	page, err := r.store.List(r.ctx, scope, sre.ListFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, inv := range page.Items {
		if inv.TriggerKind == sre.TriggerSLOBurn {
			n++
		}
	}
	return n
}

func TestSRESignals_PagePolicy(t *testing.T) {
	settings := sloSettings()
	settings.AutomaticStart = false // the default is on since the LLM-defaults change
	r := signalsFixture(t, settings)
	if len(r.sig.probes()) != 2 {
		t.Fatalf("signal probes = %d, want change_feed and slo_status", len(r.sig.probes()))
	}
	if err := r.sig.onPage(r.ctx, pageStatus("db_latency", slo.KindProxy)); err != nil {
		t.Fatal(err)
	}
	if n := burnInvestigations(t, r); n != 0 {
		t.Fatalf("a proxy burn opened %d investigations without automatic_start", n)
	}
	app := pageStatus("checkout", slo.KindApp)
	for i := 0; i < 2; i++ {
		if err := r.sig.onPage(r.ctx, app); err != nil {
			t.Fatal(err)
		}
	}
	if n := burnInvestigations(t, r); n != 1 {
		t.Fatalf("app burn opened %d investigations, want exactly 1", n)
	}
	settings.AutomaticStart = true
	auto := signalsFixture(t, settings)
	if err := auto.sig.onPage(auto.ctx, pageStatus("db_latency", slo.KindProxy)); err != nil {
		t.Fatal(err)
	}
	if n := burnInvestigations(t, auto); n != 1 {
		t.Fatalf("proxy burn with automatic_start opened %d", n)
	}
	settings.SLO.OpenInvestigations = false
	off := signalsFixture(t, settings)
	if err := off.sig.onPage(off.ctx, app); err != nil {
		t.Fatal(err)
	}
	if n := burnInvestigations(t, off); n != 0 {
		t.Fatalf("open_investigations=false opened %d", n)
	}
}

// With the SLO layer disabled the signals still carry the change feed;
// with both off there are no signal probes.
func TestSRESignals_Disabled(t *testing.T) {
	settings := config.DefaultConfig().SRE
	settings.SLO.Enabled = false
	sig := signalsFixture(t, settings).sig
	if len(sig.probes()) != 1 || sig.engine != nil || sig.feed == nil {
		t.Fatalf("slo disabled: probes=%d engine=%v", len(sig.probes()), sig.engine)
	}
	settings.ChangeEvents.FeedEnabled = false
	sig = signalsFixture(t, settings).sig
	if sig.poller != nil || sig.feed == nil {
		t.Fatalf("feed polling disabled: poller=%v feed=%v (ingested events still read)",
			sig.poller, sig.feed)
	}
}

// seedBurn writes 4 days of 5-minute cumulative samples of an app SLI
// (target 0.999) at 0.2x, the last hour at 20x.
func seedBurn(t *testing.T, r signalsRig, name string) {
	t.Helper()
	scope, _ := r.coord.Scope()
	_, err := r.pool.Exec(r.ctx, `
		INSERT INTO sage.sre_sli_samples (deployment_id, slo_name, series, observed_at,
		    bad, eligible)
		SELECT $1, $2, 'app', now() - interval '1 minute' - (1152 - g) * interval '5 minutes',
		       (CASE WHEN g > 1140 THEN (g - 1140) * 20.0 ELSE 0 END)
		           + LEAST(g, 1140) * 0.2, g * 1000.0
		FROM generate_series(1, 1152) g
		ON CONFLICT DO NOTHING`, string(scope.DeploymentID), name)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// The whole M5 chain on a live database: an app SLI burning at page
// level opens an slo_burn investigation, which collects the change feed
// (a signed deploy) and the SLO status as evidence, binds customer
// impact to the SLO evidence and asks "what changed?" with the deploy.
func TestSRESignals_BurnToInvestigationWithEvidence(t *testing.T) {
	r := signalsFixture(t, sloSettings())
	seedBurn(t, r, "checkout")
	sub := changefeed.Submission{Source: "github-actions", Kind: changefeed.KindDeploy,
		EventID: "run-" + string(sre.NewUUID()), Database: "orders",
		Summary: "deploy checkout v9", OccurredAt: time.Now().UTC().Add(-3 * time.Minute)}
	if _, _, err := r.sig.feed.Ingest(r.ctx, sub, time.Now()); err != nil {
		t.Fatal(err)
	}
	sts, err := r.sig.engine.EvaluateOnce(r.ctx)
	if err != nil {
		t.Fatalf("EvaluateOnce: %v", err)
	}
	scope, _ := r.coord.Scope()
	page, err := r.store.List(r.ctx, scope, sre.ListFilter{Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].TriggerKind != sre.TriggerSLOBurn {
		t.Fatalf("investigations = %+v err=%v (statuses %+v)", page.Items, err, sts)
	}
	if err := r.coord.Investigate(r.ctx, page.Items[0].ID); err != nil {
		t.Fatal(err)
	}
	inv, err := r.store.Get(r.ctx, scope, page.Items[0].ID)
	if err != nil || inv.Summary.CustomerImpact == nil ||
		inv.Summary.CustomerImpact.State != "burning" ||
		inv.Summary.CustomerImpact.SLO != "checkout" {
		t.Fatalf("investigation = %+v err=%v", inv, err)
	}
	hs, _ := r.store.Hypotheses(r.ctx, scope, inv.ID)
	var deploy bool
	for _, h := range hs {
		for _, f := range h.Support {
			deploy = deploy || (h.Node == "recent_change" &&
				strings.Contains(f.Text, "deploy checkout v9"))
		}
	}
	if !deploy {
		t.Fatalf("no recent_change hypothesis cites the deploy: %+v", hs)
	}
}
