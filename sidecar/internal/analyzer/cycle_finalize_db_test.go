package analyzer

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/notify"
)

// alerting-01 on real PostgreSQL: a cycle that clears a critical finding
// the analyzer paged sends exactly one resolve with the page's key.

func TestCycleClearsNotifiedCriticalSendsOneResolve(t *testing.T) {
	a, pool, d := lifecycleAnalyzer(t)
	a.WithDatabaseName("orders")
	ctx := context.Background()
	f := lifecycleFinding("test_phase2_a01_resolve", "public.orders", "critical")
	eval := map[string]bool{f.Category: true}

	a.finalizeCycle(ctx, []Finding{f}, eval)
	a.finalizeCycle(ctx, nil, eval)
	a.finalizeCycle(ctx, nil, eval)

	resolved := countFindingRows(t, pool, f.Category, f.ObjectIdentifier, "resolved")
	if resolved != 1 {
		t.Fatalf("resolved rows = %d, want 1", resolved)
	}
	if d.count() != 2 {
		t.Fatalf("dispatched %d events, want 1 trigger + 1 resolve: %+v",
			d.count(), d.events)
	}
	trig, res := d.events[0], d.events[1]
	if trig.Resolve || !res.Resolve {
		t.Fatalf("Resolve flags = %v, %v, want false, true",
			trig.Resolve, res.Resolve)
	}
	want := "finding_critical:orders:test_phase2_a01_resolve:public.orders"
	if notify.EventDedupKey(trig) != want || notify.EventDedupKey(res) != want {
		t.Fatalf("keys = %q, %q, want both %q",
			notify.EventDedupKey(trig), notify.EventDedupKey(res), want)
	}
	if res.Type != "finding_critical" || res.Severity != "critical" {
		t.Fatalf("resolve Type/Severity = %q/%q", res.Type, res.Severity)
	}
}

func TestCycleClearsWarningFindingSendsNothing(t *testing.T) {
	a, pool, d := lifecycleAnalyzer(t)
	ctx := context.Background()
	f := lifecycleFinding("test_phase2_a01_warn", "public.items", "warning")
	eval := map[string]bool{f.Category: true}
	a.finalizeCycle(ctx, []Finding{f}, eval)
	a.finalizeCycle(ctx, nil, eval)
	resolved := countFindingRows(t, pool, f.Category, f.ObjectIdentifier, "resolved")
	if resolved != 1 {
		t.Fatalf("resolved rows = %d, want 1", resolved)
	}
	if d.count() != 0 {
		t.Fatalf("dispatched %d events for a warning finding", d.count())
	}
}

// A category that was not evaluated (its producer failed) resolves
// nothing and therefore sends no resolve.
func TestCycleUnevaluatedCategorySendsNoResolve(t *testing.T) {
	a, pool, d := lifecycleAnalyzer(t)
	ctx := context.Background()
	f := lifecycleFinding("test_phase2_a01_uneval", "public.orders", "critical")
	a.finalizeCycle(ctx, []Finding{f}, map[string]bool{f.Category: true})
	a.finalizeCycle(ctx, nil, map[string]bool{})
	if got := countFindingRows(t, pool, f.Category, f.ObjectIdentifier, "open"); got != 1 {
		t.Fatalf("open rows = %d, want 1", got)
	}
	if d.count() != 1 || d.events[0].Resolve {
		t.Fatalf("events = %+v, want the trigger only", d.events)
	}
}

// State transition: page, clear, re-open within the cooldown -> the
// re-open pages again on the same key (PagerDuty opens a new incident).
func TestCycleFlapAfterResolvePagesAgain(t *testing.T) {
	a, _, d := lifecycleAnalyzer(t)
	ctx := context.Background()
	f := lifecycleFinding("test_phase2_a01_flap", "public.orders", "critical")
	eval := map[string]bool{f.Category: true}
	a.finalizeCycle(ctx, []Finding{f}, eval)
	a.finalizeCycle(ctx, nil, eval)
	a.finalizeCycle(ctx, []Finding{f}, eval)
	if d.count() != 3 {
		t.Fatalf("dispatched %d, want trigger, resolve, trigger: %+v",
			d.count(), d.events)
	}
	if d.events[0].Resolve || !d.events[1].Resolve || d.events[2].Resolve {
		t.Fatalf("actions wrong: %+v", d.events)
	}
	if notify.EventDedupKey(d.events[0]) != notify.EventDedupKey(d.events[2]) {
		t.Fatal("re-open used a different key")
	}
}

// Error propagation: a failed resolve dispatch is logged; the finding is
// still resolved in sage.findings.
func TestCycleResolveDispatchErrorIsLogged(t *testing.T) {
	a, pool, d := lifecycleAnalyzer(t)
	logs := &logCapture{}
	a.logFn = logs.logf
	ctx := context.Background()
	f := lifecycleFinding("test_phase2_a01_err", "public.orders", "critical")
	eval := map[string]bool{f.Category: true}
	a.finalizeCycle(ctx, []Finding{f}, eval)
	d.err = errors.New("webhook 500")
	a.finalizeCycle(ctx, nil, eval)
	resolved := countFindingRows(t, pool, f.Category, f.ObjectIdentifier, "resolved")
	if resolved != 1 {
		t.Fatalf("resolved rows = %d, want 1", resolved)
	}
	if !logs.contains("webhook 500") || !logs.contains("test_phase2_a01_err") {
		t.Fatalf("logs = %v", logs.lines)
	}
}

func TestResolveClearedFindings_ReturnsTransitionedRows(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanFindings(t, pool)
	t.Cleanup(func() { phase2CleanFindings(t, pool) })
	ctx := context.Background()
	keep := lifecycleFinding("test_phase2_a01_rows", "public.keep", "warning")
	gone := lifecycleFinding("test_phase2_a01_rows", "public.gone", "critical")
	if err := UpsertFindings(ctx, pool, []Finding{keep, gone}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	active := map[string]bool{keep.ObjectIdentifier: true}
	got, err := ResolveClearedFindings(ctx, pool, active, keep.Category)
	if err != nil {
		t.Fatalf("ResolveClearedFindings: %v", err)
	}
	want := ClearedFinding{Category: gone.Category,
		ObjectIdentifier: gone.ObjectIdentifier, Severity: "critical",
		Title: gone.Title}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("cleared = %+v, want [%+v]", got, want)
	}
	again, err := ResolveClearedFindings(ctx, pool, active, keep.Category)
	if err != nil || len(again) != 0 {
		t.Fatalf("second call = %+v, %v; want none", again, err)
	}
	if n := countFindingRows(t, pool, keep.Category, "public.keep", "open"); n != 1 {
		t.Fatalf("active finding open rows = %d, want 1", n)
	}
}

// Concurrent access: resolvers racing on one category report each
// cleared finding exactly once, so it is never resolved twice.
func TestResolveClearedFindings_ConcurrentReportsOnce(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanFindings(t, pool)
	t.Cleanup(func() { phase2CleanFindings(t, pool) })
	ctx := context.Background()
	cat := "test_phase2_a01_race"
	var seed []Finding
	for _, ident := range []string{"public.a", "public.b", "public.c"} {
		seed = append(seed, lifecycleFinding(cat, ident, "critical"))
	}
	if err := UpsertFindings(ctx, pool, seed); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	var wg sync.WaitGroup
	results := make([][]ClearedFinding, 4)
	errs := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = ResolveClearedFindings(ctx, pool, nil, cat)
		}(i)
	}
	wg.Wait()
	seen := map[string]int{}
	for i, r := range results {
		if errs[i] != nil {
			t.Fatalf("resolver %d: %v", i, errs[i])
		}
		for _, c := range r {
			seen[c.ObjectIdentifier]++
		}
	}
	if len(seen) != 3 || seen["public.a"] != 1 || seen["public.b"] != 1 ||
		seen["public.c"] != 1 {
		t.Fatalf("reported = %v, want each identity exactly once", seen)
	}
}
