package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/firstlook"
	"github.com/pg-sage/sidecar/internal/onboarding"
)

// A database with no catalog problems still gets setup notes (an extension
// pg_sage would like, hidden query text) and agent posture notes; none of
// them is the first finding (firstlook-07).

func setupOnlyRun(t *testing.T) (*firstLookRun, *onboarding.Tracker, context.Context) {
	t.Helper()
	_, p, ctx := firstLookDB(t)
	t.Cleanup(func() { onboarding.ForgetSetup("app") }) // Init records a process gauge
	if _, err := onboarding.Init(ctx, p, "app"); err != nil {
		t.Fatalf("onboarding init: %v", err)
	}
	tr := onboarding.NewTracker()
	started := time.Now()
	tr.Start("app", started)
	run := &firstLookRun{name: "app", pool: p, provider: "self-managed", started: started,
		tracker: tr, opts: firstlook.Options{Database: "app"},
		logf: func(string, string, ...any) {}}
	return run, tr, ctx
}

// CHECK-19: a first look whose items are only setup or posture notes
// leaves the time to first finding unset. Fails before firstlook-07.
func TestFirstLookOfSetupNotesLeavesTTFFUnset(t *testing.T) {
	run, tr, ctx := setupOnlyRun(t)
	report, err := run.execute(ctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	counts := firstlook.CountItems(report.Items)
	if len(report.Items) == 0 || counts.Findings != 0 {
		t.Fatalf("fixture first look = %+v (counts %+v), want items and no findings",
			report.Items, counts)
	}
	st, _, err := onboarding.Get(ctx, run.pool, "app")
	if err != nil || st.TTFFSeconds != nil || st.FirstFindingAt != nil {
		t.Fatalf("onboarding state = %+v err %v, want no first finding", st, err)
	}
	m := tr.Snapshot()[0]
	if m.HasTTFF || !m.FirstLookDone || m.FirstLookItems != len(report.Items) ||
		m.FirstLookFindings != 0 {
		t.Fatalf("tracker = %+v, want %d items, 0 findings, no ttff", m, len(report.Items))
	}
}

// State transition: with no qualifying finding the startup pass waits for
// the analyzer's, which then sets the time to first finding. Fails before
// firstlook-07, when the setup notes set it and the wait never ran.
func TestFirstLookOfSetupNotesWaitsForTheAnalyzer(t *testing.T) {
	run, tr, ctx := setupOnlyRun(t)
	if _, err := run.pool.Exec(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail) VALUES ('unused_index', 'warning',
		'index', 'public.x', 'unused', '{}'::jsonb)`); err != nil {
		t.Fatalf("seed analyzer finding: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		run.runOnce(ctx)
	}()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("runOnce did not return after the analyzer finding was open")
	}
	st, _, err := onboarding.Get(ctx, run.pool, "app")
	if err != nil || st.FirstFindingSource != "analyzer" || st.TTFFSeconds == nil {
		t.Fatalf("onboarding state = %+v err %v, want the analyzer's first finding", st, err)
	}
	if m := tr.Snapshot()[0]; !m.HasTTFF || m.FirstLookFindings != 0 {
		t.Fatalf("tracker = %+v, want ttff from the analyzer and 0 first-look findings", m)
	}
}

// Happy path: a qualifying catalog finding is the first finding, and the
// tracker counts it apart from the setup notes.
func TestFirstLookFindingIsTheFirstFinding(t *testing.T) {
	run, tr, ctx := setupOnlyRun(t)
	seedDuplicateIndex(t, ctx, run.pool)
	report, err := run.execute(ctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	counts := firstlook.CountItems(report.Items)
	st, _, err := onboarding.Get(ctx, run.pool, "app")
	if err != nil || st.FirstFindingSource != "first_look" || st.TTFFSeconds == nil {
		t.Fatalf("onboarding state = %+v err %v, want the first look's finding", st, err)
	}
	m := tr.Snapshot()[0]
	if counts.Findings < 1 || m.FirstLookFindings != counts.Findings ||
		m.FirstLookItems != len(report.Items) || !m.HasTTFF {
		t.Fatalf("tracker = %+v counts = %+v", m, counts)
	}
}

// CHECK-20: both first-look series are exported, items and findings apart.
func TestWriteFirstLookMetricsFindings(t *testing.T) {
	var b strings.Builder
	writeFirstLookMetrics(&b, []onboarding.Metrics{
		{Database: "app", FirstLookDone: true, FirstLookItems: 6, FirstLookFindings: 2},
		{Database: "setup", FirstLookDone: true, FirstLookItems: 4},
		{Database: "booting"},
	})
	out := b.String()
	for _, want := range []string{
		"# TYPE pg_sage_first_look_items gauge",
		"# TYPE pg_sage_first_look_findings gauge",
		`pg_sage_first_look_items{database="app"} 6`,
		`pg_sage_first_look_findings{database="app"} 2`,
		`pg_sage_first_look_items{database="setup"} 4`,
		`pg_sage_first_look_findings{database="setup"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `pg_sage_first_look_findings{database="booting"}`) {
		t.Fatalf("findings reported before the first look:\n%s", out)
	}
}
