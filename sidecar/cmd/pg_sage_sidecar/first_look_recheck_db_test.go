package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/firstlook"
	"github.com/pg-sage/sidecar/internal/onboarding"
)

// firstlook-05 on real PostgreSQL: the re-check after the operator
// follows the enablement steps, and the summary reuse across restarts.

func recheckRun(p *pgxpool.Pool) *firstLookRun {
	return &firstLookRun{name: "app", pool: p, provider: "self-managed",
		opts: firstlook.Options{Database: "app"}, logf: func(string, string, ...any) {}}
}

// extensionsStep is the checklist's Extensions step for the stored report.
func extensionsStep(t *testing.T, ctx context.Context, p *pgxpool.Pool) onboarding.Step {
	t.Helper()
	sum, found, err := firstlook.NewStore(p).LatestSummary(ctx, "app")
	if err != nil || !found {
		t.Fatalf("latest summary: found=%v err=%v", found, err)
	}
	for _, s := range onboarding.Checklist(onboarding.ChecklistInput{Connected: true,
		FirstLookReady: true, Capabilities: sum.Capabilities}) {
		if s.ID == onboarding.StepExtensions {
			return s
		}
	}
	t.Fatal("no extensions step in the checklist")
	return onboarding.Step{}
}

func capabilityStatus(r firstlook.Report, name string) string {
	for _, c := range r.Capabilities {
		if c.Name == name {
			return c.Status
		}
	}
	return ""
}

// CHECK-16: start without pg_stat_statements, create it: the Extensions
// step stays not done until the re-check, then it is done with no restart.
func TestFirstLookRecheck_ExtensionCreatedWithoutRestart(t *testing.T) {
	_, p, ctx := firstLookDB(t)
	if _, err := p.Exec(ctx, "DROP EXTENSION IF EXISTS pg_stat_statements"); err != nil {
		t.Fatalf("drop extension: %v", err)
	}
	run := recheckRun(p)
	var reprobes int
	run.reprobe = func(context.Context) { reprobes++ }
	first, err := run.execute(ctx)
	if err != nil {
		t.Fatalf("startup first look: %v", err)
	}
	if got := capabilityStatus(first, firstlook.CapStatStatements); got ==
		firstlook.CapabilityOK || got == "" {
		t.Fatalf("pg_stat_statements before CREATE EXTENSION = %q, want not ok", got)
	}
	if extensionsStep(t, ctx, p).Done {
		t.Fatal("Extensions step done without pg_stat_statements")
	}
	if _, err := p.Exec(ctx, "CREATE EXTENSION pg_stat_statements"); err != nil {
		t.Fatalf("create extension: %v", err)
	}
	if extensionsStep(t, ctx, p).Done {
		t.Fatal("Extensions step done before the re-check: the stored report changed itself")
	}
	again, err := run.Recheck(ctx)
	if err != nil {
		t.Fatalf("re-check: %v", err)
	}
	if again.ID <= first.ID {
		t.Fatalf("re-check report id %d, want a new report after %d", again.ID, first.ID)
	}
	if got := capabilityStatus(again, firstlook.CapStatStatements); got !=
		firstlook.CapabilityOK {
		t.Fatalf("pg_stat_statements after the re-check = %q, want ok", got)
	}
	step := extensionsStep(t, ctx, p)
	if !step.Done {
		t.Fatalf("Extensions step after the re-check = %+v, want done", step)
	}
	if reprobes != 1 {
		t.Fatalf("collector reprobes = %d, want 1 (only the re-check asks)", reprobes)
	}
}

// A re-check never sets the time to first finding, even when the startup
// first look found nothing and the re-check finds something.
func TestFirstLookRecheck_NeverSetsTTFF(t *testing.T) {
	_, p, ctx := firstLookDB(t)
	seedDuplicateIndex(t, ctx, p)
	t.Cleanup(func() { onboarding.ForgetSetup("app") }) // Init records a process gauge
	if _, err := onboarding.Init(ctx, p, "app"); err != nil {
		t.Fatalf("onboarding init: %v", err)
	}
	tr := onboarding.NewTracker()
	tr.Start("app", time.Now())
	run := recheckRun(p)
	run.tracker = tr
	report, err := run.Recheck(ctx)
	if err != nil {
		t.Fatalf("re-check: %v", err)
	}
	if len(report.Items) == 0 {
		t.Fatal("re-check found nothing on a duplicate index")
	}
	if snap := tr.Snapshot(); len(snap) != 1 || snap[0].HasTTFF || snap[0].FirstLookDone {
		t.Fatalf("tracker after a re-check = %+v, want it untouched", snap)
	}
	st, _, err := onboarding.Get(ctx, p, "app")
	if err != nil || st.FirstFindingAt != nil || st.TTFFSeconds != nil {
		t.Fatalf("onboarding state after a re-check = %+v err %v, want no first finding",
			st, err)
	}
}

// startupPass is what a runtime start does before it waits for the
// analyzer: the stored first look and its summary.
func startupPass(t *testing.T, ctx context.Context, run *firstLookRun) firstlook.Report {
	t.Helper()
	report, err := run.execute(ctx)
	if err != nil {
		t.Fatalf("first look: %v", err)
	}
	return run.summarize(ctx, report)
}

// CHECK-18: restarts and re-checks with an unchanged catalog reuse the
// stored summary (no model call); a changed catalog asks once.
func TestFirstLookSummary_UnchangedCatalogReusesTheSummary(t *testing.T) {
	_, p, ctx := firstLookDB(t)
	seedDuplicateIndex(t, ctx, p)
	chat := &countingChatter{reply: `{"summary":"Two indexes duplicate each other."}`}
	newRun := func() *firstLookRun {
		run := recheckRun(p)
		run.summarizer, run.model = firstlook.NewSummarizer(chat), "model-a"
		return run
	}
	first := startupPass(t, ctx, newRun())
	if chat.calls != 1 || first.Summary == "" {
		t.Fatalf("first start: calls %d summary %q, want one model call", chat.calls,
			first.Summary)
	}
	for restart := 1; restart <= 2; restart++ {
		r := startupPass(t, ctx, newRun())
		if chat.calls != 1 {
			t.Fatalf("restart %d: model calls = %d, want none past the first", restart,
				chat.calls-1)
		}
		stored, _, err := firstlook.NewStore(p).Latest(ctx, "app")
		if err != nil || stored.ID != r.ID || stored.Summary != first.Summary ||
			stored.SummaryModel != "model-a" {
			t.Fatalf("restart %d stored %+v err %v, want the reused summary", restart,
				stored, err)
		}
	}
	run := newRun()
	if _, err := run.Recheck(ctx); err != nil || chat.calls != 1 {
		t.Fatalf("unchanged re-check: calls %d err %v, want no model call", chat.calls, err)
	}
	if _, err := p.Exec(ctx, "DROP INDEX public.fl_orders_c2"); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	changed, err := run.Recheck(ctx)
	if err != nil || chat.calls != 2 {
		t.Fatalf("changed re-check: calls %d err %v, want exactly one more", chat.calls, err)
	}
	if changed.Summary == "" {
		t.Fatal("changed re-check returned no summary")
	}
}

// A failed first look leaves the previous report as the latest.
func TestFirstLookRecheck_FailureKeepsThePreviousReport(t *testing.T) {
	dsn, p, ctx := firstLookDB(t)
	before, err := recheckRun(p).execute(ctx)
	if err != nil {
		t.Fatalf("first look: %v", err)
	}
	closed, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("second pool: %v", err)
	}
	closed.Close()
	if _, err := recheckRun(closed).Recheck(ctx); err == nil {
		t.Fatal("re-check on a closed pool succeeded")
	} else if errors.Is(err, firstlook.ErrNoPool) {
		t.Fatalf("err = %v, want the connection failure, not ErrNoPool", err)
	}
	latest, found, err := firstlook.NewStore(p).Latest(ctx, "app")
	if err != nil || !found || latest.ID != before.ID {
		t.Fatalf("latest after a failed re-check: id %d found %v err %v, want %d",
			latest.ID, found, err, before.ID)
	}
}
