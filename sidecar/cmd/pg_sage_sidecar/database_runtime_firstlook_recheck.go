package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/firstlook"
)

// firstlook-05: after the operator follows the enablement steps (CREATE
// EXTENSION, a GRANT), POST /api/v1/first-look/recheck runs the same
// bounded, read-only first look again, so the checklist and the degraded
// checks catch up without a restart. Re-checks are operator-started only:
// there is no periodic re-probe.

const (
	// firstLookRecheckBudget bounds a whole re-check (re-probe, catalog
	// pass, retry, summary) below the API server's 60 s write timeout.
	firstLookRecheckBudget = 45 * time.Second
	// firstLookRecheckRetryDelay replaces the startup delay: a re-check
	// runs long after pg_sage's own startup reads have settled.
	firstLookRecheckRetryDelay = 2 * time.Second
)

// passGate admits one first look pass at a time; its zero value is open.
type passGate struct {
	once sync.Once
	ch   chan struct{}
}

func (g *passGate) slot() chan struct{} {
	g.once.Do(func() { g.ch = make(chan struct{}, 1) })
	return g.ch
}

// acquire waits for the gate or for ctx to end.
func (g *passGate) acquire(ctx context.Context) error {
	select {
	case g.slot() <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *passGate) release() { <-g.slot() }

// recheckFlight runs one re-check pass at a time and gives its result to
// every caller that asked while it ran.
type recheckFlight struct {
	mu   sync.Mutex
	call *recheckCall
}

type recheckCall struct {
	done   chan struct{}
	report firstlook.Report
	err    error
}

// do joins the pass in flight or starts pass. The pass runs on its own
// goroutine, so a caller whose request ends does not cancel it for the
// others; it returns its own context's error instead.
func (fl *recheckFlight) do(ctx context.Context,
	pass func() (firstlook.Report, error)) (firstlook.Report, error) {
	fl.mu.Lock()
	c := fl.call
	if c == nil {
		c = &recheckCall{done: make(chan struct{})}
		fl.call = c
		go func() {
			c.report, c.err = pass()
			fl.mu.Lock()
			fl.call = nil
			fl.mu.Unlock()
			close(c.done)
		}()
	}
	fl.mu.Unlock()
	select {
	case <-c.done:
		return c.report, c.err
	case <-ctx.Done():
		return firstlook.Report{}, ctx.Err()
	}
}

// inFlight reports whether a pass is running.
func (fl *recheckFlight) inFlight() bool {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	return fl.call != nil
}

// Recheck runs the first look again and returns the stored report. It
// waits for a pass already running (the startup first look), shares a
// re-check in flight, and never sets the time to first finding. A failed
// first look stores nothing, so the previous report stays the latest.
func (f *firstLookRun) Recheck(ctx context.Context) (firstlook.Report, error) {
	life := f.life
	if life == nil {
		life = context.Background()
	}
	if err := life.Err(); err != nil {
		return firstlook.Report{}, fmt.Errorf("first look of %q: runtime stopped: %w",
			f.name, err)
	}
	return f.flight.do(ctx, func() (firstlook.Report, error) {
		pctx, cancel := context.WithTimeout(life, firstLookRecheckBudget)
		defer cancel()
		return f.recheckPass(pctx)
	})
}

// recheckPass is one re-check under pctx, which also bounds the wait for a
// pass in progress.
func (f *firstLookRun) recheckPass(pctx context.Context) (firstlook.Report, error) {
	if err := f.gate.acquire(pctx); err != nil {
		return firstlook.Report{}, fmt.Errorf("first look of %q: wait for the pass in "+
			"progress: %w", f.name, err)
	}
	defer f.gate.release()
	if f.reprobe != nil {
		f.reprobe(pctx)
	}
	report, err := f.catalogPass(pctx, nil)
	if err != nil {
		return firstlook.Report{}, fmt.Errorf("first look of %q: %w", f.name, err)
	}
	report = f.retryWith(pctx, report, firstLookRecheckRetryDelay, nil)
	f.logf("INFO", "first look of %q: re-checked on request, %s", f.name,
		itemsNote(report.Items))
	return f.summarize(pctx, report), nil
}

// summaryFingerprint is what a summary depends on: each item's section,
// rule, object and severity, and the checks that could not run, sorted.
// Titles and details (counts, sizes) do not change it. Empty when there is
// nothing to summarize.
func summaryFingerprint(r firstlook.Report) string {
	var parts []string
	for _, it := range r.Items {
		parts = append(parts, strings.Join([]string{"item", it.Section, it.Rule,
			it.Object, it.Severity}, "\x1f"))
	}
	for _, c := range r.Checks {
		if c.Status == firstlook.CheckDegraded {
			parts = append(parts, strings.Join([]string{"degraded", c.Section, c.Rule},
				"\x1f"))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x1e")
}

// reuseSummary copies the previous report's summary and model onto report
// when both have the same fingerprint, so a restart or a re-check of an
// unchanged catalog makes no model call. ok is false when the model must
// be asked (nothing comparable, a change, or the previous one unreadable).
func (f *firstLookRun) reuseSummary(ctx context.Context,
	report firstlook.Report) (firstlook.Report, bool) {
	fp := summaryFingerprint(report)
	if fp == "" {
		return report, false
	}
	store := firstlook.NewStore(f.pool)
	prev, found, err := store.Previous(ctx, report.Database, report.ID)
	if err != nil {
		f.logf("WARN", "first look of %q: read the previous report to reuse its "+
			"summary: %v", f.name, err)
		return report, false
	}
	if !found || prev.Summary == "" || summaryFingerprint(prev) != fp {
		return report, false
	}
	if err := store.SetSummary(ctx, report.ID, prev.Summary, prev.SummaryModel); err != nil {
		f.logf("WARN", "first look of %q: save the reused summary: %v", f.name, err)
		return report, false
	}
	report.Summary, report.SummaryModel = prev.Summary, prev.SummaryModel
	f.logf("INFO", "first look of %q: unchanged since report %d, summary reused "+
		"without a model call", f.name, prev.ID)
	return report, true
}

// firstLookRecheck is the fleet instance's re-check hook; nil when the
// runtime takes no first look.
func (rt *databaseRuntime) firstLookRecheck() func(context.Context) (
	firstlook.Report, error) {
	if rt.firstLook == nil {
		return nil
	}
	return rt.firstLook.Recheck
}

// reprobeCapabilities makes a re-check and the collector agree: the
// collector re-probes pg_stat_statements now, and the fleet capability
// refresher is asked for a prompt (non-blocking) recollection.
func (rt *databaseRuntime) reprobeCapabilities(ctx context.Context) {
	if rt.collector != nil {
		rt.collector.ReprobeCapabilities(ctx)
	}
	if rt.inst != nil && rt.inst.CapabilityRefresh != nil {
		rt.inst.CapabilityRefresh()
	}
}
