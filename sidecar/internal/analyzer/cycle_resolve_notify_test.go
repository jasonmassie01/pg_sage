package analyzer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/notify"
)

// alerting-01 unit tests: which cleared findings send a PagerDuty
// resolve, and what happens when resolving fails. The real-PostgreSQL
// lifecycle is in cycle_finalize_db_test.go.

// logCapture records formatted log lines; safe for concurrent use.
type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (l *logCapture) logf(level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(format, args...))
}

func (l *logCapture) contains(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.Contains(line, sub) {
			return true
		}
	}
	return false
}

func resolveNotifyAnalyzer(
	pool *pgxpool.Pool,
) (*Analyzer, *mockDispatcher, *logCapture) {
	logs := &logCapture{}
	d := &mockDispatcher{}
	a := New(pool, coverageTestConfig(), nil, nil, nil, nil, nil, logs.logf)
	a.WithDispatcher(d)
	a.WithDatabaseName("orders")
	return a, d, logs
}

func TestDispatchResolvedCritical_OnlyCriticalRowsResolve(t *testing.T) {
	a, d, _ := resolveNotifyAnalyzer(nil)
	a.dispatchResolvedCritical(context.Background(), []ClearedFinding{
		{Category: "xid_wraparound", ObjectIdentifier: "current_database",
			Severity: "critical", Title: "XID wraparound"},
		{Category: "bloat", ObjectIdentifier: "public.t",
			Severity: "warning", Title: "bloat"},
		{Category: "unused_index", ObjectIdentifier: "public.i",
			Severity: "info", Title: "unused"},
	})
	if d.count() != 1 {
		t.Fatalf("dispatched %d events, want 1 (critical only)", d.count())
	}
	evt := d.events[0]
	want := "finding_critical:orders:xid_wraparound:current_database"
	if !evt.Resolve || evt.Type != "finding_critical" ||
		notify.EventDedupKey(evt) != want {
		t.Fatalf("event = %+v, want resolve keyed %q", evt, want)
	}
}

// The resolve's key equals the key of the page the analyzer sent for the
// same finding, whatever the title said at either time.
func TestDispatchResolvedCritical_KeyMatchesTrigger(t *testing.T) {
	a, d, _ := resolveNotifyAnalyzer(nil)
	ctx := context.Background()
	f := Finding{Category: "xid_wraparound", ObjectIdentifier: "current_database",
		Severity: "critical", Title: "XID age 1.9B",
		Detail: map[string]any{"age": 1}}
	a.dispatchCriticalFindings(ctx, []Finding{f})
	a.dispatchResolvedCritical(ctx, []ClearedFinding{{Category: f.Category,
		ObjectIdentifier: f.ObjectIdentifier, Severity: "critical",
		Title: "XID age 2.0B"}})
	if d.count() != 2 {
		t.Fatalf("dispatched %d events, want 2", d.count())
	}
	trig, res := d.events[0], d.events[1]
	if trig.Resolve || !res.Resolve {
		t.Fatalf("Resolve flags = %v, %v, want false, true",
			trig.Resolve, res.Resolve)
	}
	if notify.EventDedupKey(trig) != notify.EventDedupKey(res) {
		t.Fatalf("trigger key %q != resolve key %q",
			notify.EventDedupKey(trig), notify.EventDedupKey(res))
	}
	if trig.Data["category"] != "xid_wraparound" ||
		trig.Data["object"] != "current_database" {
		t.Fatalf("trigger Data = %v, want category and object", trig.Data)
	}
}

// State transition: a finding paged at critical and later de-escalated
// to warning while still open must still resolve its page when it clears.
func TestDispatchResolvedCritical_PagedThenDeescalatedResolves(t *testing.T) {
	a, d, _ := resolveNotifyAnalyzer(nil)
	ctx := context.Background()
	f := Finding{Category: "conn", ObjectIdentifier: "max", Severity: "critical"}
	paged := a.notifiableCritical(UpsertResult{Opened: []Finding{f}}, time.Now())
	a.dispatchCriticalFindings(ctx, paged)
	a.dispatchResolvedCritical(ctx, []ClearedFinding{{Category: "conn",
		ObjectIdentifier: "max", Severity: "warning", Title: "conn"}})
	if d.count() != 2 || !d.events[1].Resolve {
		t.Fatalf("events = %+v, want trigger then resolve", d.events)
	}
}

// State transition: after a resolve, a re-opened critical finding pages
// again even inside the cooldown, or PagerDuty would show it resolved.
func TestDispatchResolvedCritical_ResetsCooldown(t *testing.T) {
	a, _, _ := resolveNotifyAnalyzer(nil)
	ctx := context.Background()
	now := time.Now()
	f := Finding{Category: "conn", ObjectIdentifier: "max", Severity: "critical"}
	opened := UpsertResult{Opened: []Finding{f}}
	if got := a.notifiableCritical(opened, now); len(got) != 1 {
		t.Fatalf("first open notifiable = %d, want 1", len(got))
	}
	if got := a.notifiableCritical(opened, now); len(got) != 0 {
		t.Fatalf("cooldown not applied: %d notifiable", len(got))
	}
	a.dispatchResolvedCritical(ctx, []ClearedFinding{{Category: "conn",
		ObjectIdentifier: "max", Severity: "critical"}})
	if got := a.notifiableCritical(opened, now); len(got) != 1 {
		t.Fatalf("reopen after resolve notifiable = %d, want 1", len(got))
	}
}

// Nil/empty: no dispatcher, no rows, or a nil slice are all no-ops.
func TestDispatchResolvedCritical_NilAndEmpty(t *testing.T) {
	a := New(nil, coverageTestConfig(), nil, nil, nil, nil, nil, noopLog)
	a.dispatchResolvedCritical(context.Background(), []ClearedFinding{
		{Category: "c", ObjectIdentifier: "o", Severity: "critical"}})
	b, d, _ := resolveNotifyAnalyzer(nil)
	b.dispatchResolvedCritical(context.Background(), nil)
	b.dispatchResolvedCritical(context.Background(), []ClearedFinding{})
	if d.count() != 0 {
		t.Fatalf("dispatched %d events for no rows", d.count())
	}
}

// Error propagation: a failed dispatch is logged with the finding's
// identity and does not stop the remaining resolves.
func TestDispatchResolvedCritical_DispatchErrorLoggedAndContinues(t *testing.T) {
	a, d, logs := resolveNotifyAnalyzer(nil)
	d.err = errors.New("pagerduty: 503 service unavailable")
	a.dispatchResolvedCritical(context.Background(), []ClearedFinding{
		{Category: "a", ObjectIdentifier: "o1", Severity: "critical"},
		{Category: "b", ObjectIdentifier: "o2", Severity: "critical"},
	})
	if d.count() != 2 {
		t.Fatalf("dispatched %d, want 2 (an error must not stop the loop)",
			d.count())
	}
	if !logs.contains("ERROR") || !logs.contains("a/o1") ||
		!logs.contains("503 service unavailable") {
		t.Fatalf("logs = %v, want ERROR naming a/o1 and the cause", logs.lines)
	}
}

// Error propagation: when ResolveCleared cannot read sage.findings the
// error is logged with the category and no resolve is sent.
func TestResolveCleared_QueryFailureLoggedNoResolve(t *testing.T) {
	pool, err := pgxpool.New(context.Background(),
		"postgres://sage@127.0.0.1:1/sage?connect_timeout=1")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	pool.Close() // every query now fails with "closed pool"
	a, d, logs := resolveNotifyAnalyzer(pool)
	a.resolveCleared(context.Background(), nil,
		map[string]bool{"xid_wraparound": true})
	if d.count() != 0 {
		t.Fatalf("dispatched %d resolves after a failed query", d.count())
	}
	if !logs.contains("resolve cleared xid_wraparound") {
		t.Fatalf("logs = %v, want the category in the error", logs.lines)
	}
}
