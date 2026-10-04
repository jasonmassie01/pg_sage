package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/firstlook"
	"github.com/pg-sage/sidecar/internal/onboarding"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/startup"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// firstLookDB creates a disposable database with a bootstrapped sage
// schema and returns its DSN and a pool.
func firstLookDB(t *testing.T) (string, *pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.CreateDatabase(t, "firstlook")
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	t.Cleanup(cancel)
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(p.Close)
	if err := schema.Bootstrap(ctx, p); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return dsn, p, ctx
}

func seedDuplicateIndex(t *testing.T, ctx context.Context, p *pgxpool.Pool) {
	t.Helper()
	for _, s := range []string{
		"CREATE TABLE public.fl_orders (id int PRIMARY KEY, customer int)",
		"CREATE INDEX fl_orders_c1 ON public.fl_orders (customer)",
		"CREATE INDEX fl_orders_c2 ON public.fl_orders (customer)",
	} {
		if _, err := p.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func TestFirstLookRunPersistsMeasuresAndRecords(t *testing.T) {
	_, p, ctx := firstLookDB(t)
	seedDuplicateIndex(t, ctx, p)
	if _, err := onboarding.Init(ctx, p, "app"); err != nil {
		t.Fatalf("onboarding init: %v", err)
	}
	tr := onboarding.NewTracker()
	started := time.Now()
	tr.Start("app", started)
	run := &firstLookRun{name: "app", pool: p, provider: "self-managed", started: started,
		tracker: tr, facts: facts.NewStore(p), opts: firstlook.Options{Database: "app"},
		logf: func(string, string, ...any) {}}
	report, err := run.execute(ctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(report.Items) == 0 || report.ID <= 0 {
		t.Fatalf("report = %+v, want saved items", report)
	}
	saved, found, err := firstlook.NewStore(p).Latest(ctx, "app")
	if err != nil || !found || saved.ID != report.ID {
		t.Fatalf("latest: found=%v id=%d err=%v", found, saved.ID, err)
	}
	st, _, err := onboarding.Get(ctx, p, "app")
	if err != nil || st.FirstLookAt == nil || st.FirstFindingAt == nil ||
		st.FirstFindingSource != "first_look" || st.TTFFSeconds == nil || *st.TTFFSeconds <= 0 {
		t.Fatalf("onboarding state = %+v err %v", st, err)
	}
	snap := tr.Snapshot()
	if len(snap) != 1 || !snap[0].HasTTFF || snap[0].FirstLookItems != len(report.Items) ||
		snap[0].TTFF > time.Since(started) {
		t.Fatalf("tracker = %+v", snap)
	}
	stored, err := facts.NewStore(p).List(ctx, facts.Filter{})
	if err != nil || len(stored) < len(report.FactProposals) {
		t.Fatalf("facts stored %d err %v, want the %d first-look proposals", len(stored),
			err, len(report.FactProposals))
	}
}

func TestFirstLookAwaitsAnAnalyzerFindingWhenEmpty(t *testing.T) {
	_, p, ctx := firstLookDB(t)
	if _, err := onboarding.Init(ctx, p, "app"); err != nil {
		t.Fatalf("onboarding init: %v", err)
	}
	tr := onboarding.NewTracker()
	started := time.Now()
	tr.Start("app", started)
	tr.FirstLook("app", started, 0, time.Millisecond)
	run := &firstLookRun{name: "app", pool: p, started: started, tracker: tr,
		logf: func(string, string, ...any) {}}
	go func() {
		time.Sleep(300 * time.Millisecond)
		_, _ = p.Exec(context.Background(), `INSERT INTO sage.findings (category, severity,
			object_type, object_identifier, title, detail) VALUES ('unused_index', 'info',
			'index', 'public.x', 'unused', '{}'::jsonb)`)
	}()
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if !run.awaitFirstFinding(wctx, 50*time.Millisecond) {
		t.Fatal("no first finding observed")
	}
	st, _, err := onboarding.Get(ctx, p, "app")
	if err != nil || st.FirstFindingSource != "analyzer" || st.TTFFSeconds == nil {
		t.Fatalf("state = %+v err %v", st, err)
	}
	if m := tr.Snapshot()[0]; !m.HasTTFF || m.TTFF < 300*time.Millisecond {
		t.Fatalf("tracker = %+v, want a ttff of at least 300ms", m)
	}
	// A cancelled wait returns without recording.
	cctx, ccancel := context.WithCancel(ctx)
	ccancel()
	if run.awaitFirstFinding(cctx, time.Millisecond) {
		t.Fatal("a cancelled wait reported a finding")
	}
}

func TestFirstLookExecuteErrors(t *testing.T) {
	run := &firstLookRun{name: "app", tracker: onboarding.NewTracker(),
		logf: func(string, string, ...any) {}}
	if _, err := run.execute(context.Background()); !errors.Is(err, firstlook.ErrNoPool) {
		t.Fatalf("nil pool err = %v, want ErrNoPool", err)
	}
}

func TestWriteFirstLookMetrics(t *testing.T) {
	var b strings.Builder
	writeFirstLookMetrics(&b, []onboarding.Metrics{
		{Database: "app", FirstLookDone: true, FirstLookItems: 4,
			FirstLookDuration: 1500 * time.Millisecond, HasTTFF: true, TTFF: 9 * time.Second},
		{Database: "idle", FirstLookDone: true},
		{Database: "booting"},
	})
	out := b.String()
	for _, want := range []string{
		"# TYPE pg_sage_time_to_first_finding_seconds gauge",
		`pg_sage_time_to_first_finding_seconds{database="app"} 9`,
		`pg_sage_first_look_items{database="app"} 4`,
		`pg_sage_first_look_duration_seconds{database="app"} 1.5`,
		`pg_sage_first_look_items{database="idle"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics lack %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{`time_to_first_finding_seconds{database="idle"}`,
		`{database="booting"}`} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("metrics report %q before it is known:\n%s", unwanted, out)
		}
	}
	var empty strings.Builder
	writeFirstLookMetrics(&empty, nil)
	if empty.Len() != 0 {
		t.Fatalf("no databases wrote %q", empty.String())
	}
}

// buildOnboardingRuntime builds a YAML-fleet runtime for dsn with the
// given trust level ("" keeps the default) and returns its executor trust.
func buildOnboardingRuntime(t *testing.T, dsn, trust string) string {
	t.Helper()
	base := parityBaseConfig(t, dsn)
	base.Trust = config.DefaultConfig().Trust
	if trust != "" {
		base.Trust.Level = trust
	}
	preserveParityGlobals(t, base)
	cfg.Mode = "fleet"
	name := base.Postgres.Database
	cfg.Databases = []config.DatabaseConfig{parityDatabaseConfig(base, name)}
	initFleetMultiDB()
	inst := fleetMgr.GetInstance(name)
	if inst == nil || inst.Executor == nil {
		t.Fatal("fleet registered no runtime")
	}
	return inst.Executor.TrustLevel()
}

func TestNewInstallStartsReadOnlyWithAFirstLook(t *testing.T) {
	dsn, p, ctx := firstLookDB(t)
	seedDuplicateIndex(t, ctx, p)
	if got := buildOnboardingRuntime(t, dsn, ""); got != "observation" {
		t.Fatalf("new install trust = %q, want observation", got)
	}
	var report firstlook.Report
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		r, found, err := firstlook.NewStore(p).Latest(ctx, testDatabaseName(t, p))
		if err == nil && found {
			report = r
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(report.Items) == 0 {
		t.Fatal("no first look within 60 s of a new install")
	}
	st, _, err := onboarding.Get(ctx, p, testDatabaseName(t, p))
	if err != nil || st.InstallKind != onboarding.InstallNew {
		t.Fatalf("onboarding = %+v err %v, want a new install", st, err)
	}
	var actions int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM sage.action_log").Scan(&actions); err != nil ||
		actions != 0 {
		t.Fatalf("actions = %d err %v: a read-only install must not act", actions, err)
	}
}

func TestExistingInstallKeepsConfiguredTrust(t *testing.T) {
	dsn, p, ctx := firstLookDB(t)
	if _, err := p.Exec(ctx, `INSERT INTO sage.config (key, value, updated_by)
		VALUES ('trust_ramp_start', '2026-01-01T00:00:00Z', 'bootstrap')`); err != nil {
		t.Fatalf("ramp row: %v", err)
	}
	if got := buildOnboardingRuntime(t, dsn, "advisory"); got != "advisory" {
		t.Fatalf("upgraded install trust = %q, want its configured advisory", got)
	}
	st, _, err := onboarding.Get(ctx, p, testDatabaseName(t, p))
	if err != nil || st.InstallKind != onboarding.InstallExisting {
		t.Fatalf("onboarding = %+v err %v, want an existing install", st, err)
	}
}

func TestPrepareMonitoredDatabaseDegradesMissingStatementsInStandalone(t *testing.T) {
	stubPreparation(t, nil, fmt.Errorf("pg_stat_statements check: %w",
		startup.ErrStatementsUnavailable))
	checks, err := prepareMonitoredDatabase(context.Background(), nil, "orders", true)
	if err != nil {
		t.Fatalf("missing pg_stat_statements refused the database: %v", err)
	}
	if checks == nil || checks.PGVersionNum != 160004 || checks.QueryTextVisible {
		t.Fatalf("degraded checks = %+v, want version probed and query text off", checks)
	}
}

func testDatabaseName(t *testing.T, p *pgxpool.Pool) string {
	t.Helper()
	return p.Config().ConnConfig.Database
}
