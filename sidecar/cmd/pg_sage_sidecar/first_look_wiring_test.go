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
	t.Cleanup(func() { onboarding.ForgetSetup("app") }) // Init records a process gauge
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
	t.Cleanup(func() { onboarding.ForgetSetup("app") }) // Init records a process gauge
	if _, err := onboarding.Init(ctx, p, "app"); err != nil {
		t.Fatalf("onboarding init: %v", err)
	}
	tr := onboarding.NewTracker()
	started := time.Now()
	tr.Start("app", started)
	tr.FirstLook("app", started, 0, 0, time.Millisecond)
	run := &firstLookRun{name: "app", pool: p, started: started, tracker: tr,
		logf: func(string, string, ...any) {}}
	go func() {
		time.Sleep(300 * time.Millisecond)
		_, _ = p.Exec(context.Background(), `INSERT INTO sage.findings (category, severity,
			object_type, object_identifier, title, detail) VALUES ('unused_index', 'warning',
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

func TestItemsNoteSingularAndPlural(t *testing.T) {
	warn := firstlook.Item{Rule: "duplicate_index", Severity: firstlook.SeverityWarning}
	note := firstlook.Item{Rule: "duplicate_index", Severity: firstlook.SeverityInfo}
	for _, tc := range []struct {
		items []firstlook.Item
		want  string
	}{
		{nil, "0 findings, 0 items"},
		{[]firstlook.Item{}, "0 findings, 0 items"},
		{[]firstlook.Item{note}, "0 findings, 1 item"},
		{[]firstlook.Item{warn}, "1 finding, 1 item"},
		{[]firstlook.Item{warn, note}, "1 finding, 2 items"},
		{[]firstlook.Item{warn, warn}, "2 findings, 2 items"},
	} {
		if got := itemsNote(tc.items); got != tc.want {
			t.Errorf("itemsNote(%d items) = %q, want %q", len(tc.items), got, tc.want)
		}
	}
}

// count is how many recorded lines contain substr (the recorder is
// sre_signals_owner_test.go's); awaitFirstFinding logs from its goroutine.
func (l *logRecorder) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

func insertAnalyzerFinding(t *testing.T, ctx context.Context, p *pgxpool.Pool, sev string) {
	t.Helper()
	if _, err := p.Exec(ctx, `INSERT INTO sage.findings (category, severity, object_type,
		object_identifier, title, detail) VALUES ('unused_index', $1, 'index',
		'public.x_' || $1, 'unused', '{}'::jsonb)`, sev); err != nil {
		t.Fatalf("insert %s finding: %v", sev, err)
	}
}

// awaitRun starts an analyzer wait for "app" after an empty first look.
func awaitRun(t *testing.T, ctx context.Context, p *pgxpool.Pool) (*firstLookRun,
	*logRecorder) {
	t.Helper()
	t.Cleanup(func() { onboarding.ForgetSetup("app") }) // Init records a process gauge
	if _, err := onboarding.Init(ctx, p, "app"); err != nil {
		t.Fatalf("onboarding init: %v", err)
	}
	tr := onboarding.NewTracker()
	started := time.Now()
	tr.Start("app", started)
	tr.FirstLook("app", started, 0, 0, time.Millisecond)
	logs := &logRecorder{}
	return &firstLookRun{name: "app", pool: p, started: started, tracker: tr,
		logf: logs.log}, logs
}

// TestAwaitFirstFindingWaitsPastAnInfoNote pins firstlook-07b: an info note
// is not the first finding; the wait goes on until a warning arrives, which
// records the time to first finding once, from the analyzer.
func TestAwaitFirstFindingWaitsPastAnInfoNote(t *testing.T) {
	_, p, ctx := firstLookDB(t)
	run, logs := awaitRun(t, ctx, p)
	insertAnalyzerFinding(t, ctx, p, "info")
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	done := make(chan bool, 1)
	go func() { done <- run.awaitFirstFinding(wctx, 20*time.Millisecond) }()
	select {
	case got := <-done:
		t.Fatalf("an info note ended the wait (returned %v)", got)
	case <-time.After(400 * time.Millisecond):
	}
	if st, _, err := onboarding.Get(ctx, p, "app"); err != nil || st.FirstFindingAt != nil {
		t.Fatalf("state after an info note = %+v err %v, want no first finding", st, err)
	}
	if m := run.tracker.Snapshot()[0]; m.HasTTFF {
		t.Fatalf("tracker after an info note = %+v, want no ttff", m)
	}
	warnAt := time.Now()
	insertAnalyzerFinding(t, ctx, p, "warning")
	if !<-done {
		t.Fatal("the warning was not seen as the first finding")
	}
	st, _, err := onboarding.Get(ctx, p, "app")
	if err != nil || st.FirstFindingSource != "analyzer" || st.FirstFindingAt == nil ||
		st.FirstFindingAt.Before(warnAt.Add(-time.Second)) || st.TTFFSeconds == nil {
		t.Fatalf("state = %+v err %v, want an analyzer first finding after %s", st, err, warnAt)
	}
	if m := run.tracker.Snapshot()[0]; !m.HasTTFF || m.TTFF < warnAt.Sub(run.started) {
		t.Fatalf("tracker = %+v, want a ttff of at least %s", m, warnAt.Sub(run.started))
	}
	first := *st.FirstFindingAt
	if !run.awaitFirstFinding(wctx, 20*time.Millisecond) {
		t.Fatal("second wait did not see the open warning")
	}
	again, _, err := onboarding.Get(ctx, p, "app")
	if err != nil || again.FirstFindingAt == nil || !again.FirstFindingAt.Equal(first) {
		t.Fatalf("second wait changed the first finding: %+v err %v", again, err)
	}
	if n := logs.count("first finding (analyzer)"); n != 1 {
		t.Fatalf("first finding logged %d times, want once: %v", n, logs.lines)
	}
}

// TestAwaitFirstFindingLogsReadErrorsAndKeepsPolling: a failed read logs
// one WARN per poll and the wait goes on; it ends at the first warning once
// the table reads again.
func TestAwaitFirstFindingLogsReadErrorsAndKeepsPolling(t *testing.T) {
	_, p, ctx := firstLookDB(t)
	run, logs := awaitRun(t, ctx, p)
	if _, err := p.Exec(ctx, "ALTER TABLE sage.findings RENAME TO findings_gone"); err != nil {
		t.Fatalf("hide findings: %v", err)
	}
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	done := make(chan bool, 1)
	go func() { done <- run.awaitFirstFinding(wctx, 20*time.Millisecond) }()
	deadline := time.Now().Add(10 * time.Second)
	for logs.count("read open findings") < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case got := <-done:
		t.Fatalf("a read error ended the wait (returned %v)", got)
	default:
	}
	if _, err := p.Exec(ctx, "ALTER TABLE sage.findings_gone RENAME TO findings"); err != nil {
		t.Fatalf("restore findings: %v", err)
	}
	insertAnalyzerFinding(t, ctx, p, "critical")
	if !<-done {
		t.Fatal("the wait did not resume after the read error")
	}
	warns, reads := logs.count("WARN "), logs.count("read open findings")
	if reads < 2 || warns != reads ||
		logs.count(`WARN db "app": watch for the first finding: read open findings`) != reads {
		t.Fatalf("logs = %v, want one WARN per failed poll (at least 2)", logs.lines)
	}
	if st, _, err := onboarding.Get(ctx, p, "app"); err != nil ||
		st.FirstFindingSource != "analyzer" {
		t.Fatalf("state = %+v err %v, want an analyzer first finding", st, err)
	}
}

// No concurrent-access test: one goroutine per database polls, and the
// change is the SQL predicate only (the log recorder above is locked).
