package sre

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// scriptedRunner answers probes from scripts: each call of a probe takes
// the next scripted result (the last one repeats). It never touches a
// database; hook, when set, runs before a probe answers.
type scriptedRunner struct {
	mu      sync.Mutex
	scripts map[probes.ID][]probes.Result
	calls   map[probes.ID]int
	hook    func(id probes.ID, call int)
}

func newScriptedRunner() *scriptedRunner {
	return &scriptedRunner{scripts: map[probes.ID][]probes.Result{},
		calls: map[probes.ID]int{}}
}

func (r *scriptedRunner) script(id probes.ID, rs ...probes.Result) *scriptedRunner {
	r.scripts[id] = rs
	return r
}

func (r *scriptedRunner) Run(_ context.Context, id probes.ID, _ probes.Args) probes.Result {
	r.mu.Lock()
	n := r.calls[id]
	r.calls[id] = n + 1
	hook := r.hook
	rs := r.scripts[id]
	r.mu.Unlock()
	if hook != nil {
		hook(id, n)
	}
	if len(rs) == 0 {
		return probes.Result{ProbeID: id, Version: "v1", Status: probes.StatusEmpty,
			Reason: "no_rows", ObservedAt: time.Now()}
	}
	res := rs[min(n, len(rs)-1)]
	res.ObservedAt = time.Now()
	return res
}

func (r *scriptedRunner) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		n += c
	}
	return n
}

func rows(id probes.ID, rs ...probes.Row) probes.Result {
	st := probes.StatusOK
	if len(rs) == 0 {
		st = probes.StatusEmpty
	}
	return probes.Result{ProbeID: id, Version: "v1", Status: st, Rows: rs}
}

// idleChainRunner scripts an idle-in-transaction holder (pid 4242) with
// an ALTER queued behind it and a reader behind the ALTER.
func idleChainRunner() *scriptedRunner {
	start := time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)
	edge := func(waiter, blocker int64, mode, state string, age float64,
		waiting bool) probes.Row {
		return probes.Row{"waiter_pid": waiter, "lock_type": "relation",
			"requested_mode": mode, "relation": "public.orders", "blocker_pid": blocker,
			"blocker_kind": "backend", "blocker_state": state, "blocker_waiting": waiting,
			"blocker_xact_age_s": age, "blocker_backend_start": start}
	}
	return newScriptedRunner().
		script(probes.LockGraph, rows(probes.LockGraph,
			edge(20, 4242, "AccessExclusiveLock", "idle in transaction", 90, false),
			edge(30, 20, "AccessShareLock", "active", 1, true))).
		script(probes.PreparedXacts, rows(probes.PreparedXacts))
}

func connRow(app, state string, n int64, total int64) probes.Row {
	return probes.Row{"in_current_database": true, "application_name": app,
		"client_addr": "10.0.0.5", "state": state, "backends": n,
		"waiting_on_lock": int64(0), "max_connections": int64(100),
		"reserved_connections": int64(3), "total_client_backends": total,
		"server_started_at": time.Date(2026, 9, 27, 6, 0, 0, 0, time.UTC)}
}

// leakRunner scripts an application whose idle pool grows from 5 to 14
// between the two connection samples.
func leakRunner() *scriptedRunner {
	return newScriptedRunner().script(probes.ConnectionSaturation,
		rows(probes.ConnectionSaturation, connRow("worker", "idle", 5, 10)),
		rows(probes.ConnectionSaturation, connRow("worker", "idle", 14, 19)))
}

// testCoordinator builds a coordinator on a fresh scope with instant
// sample delays that are recorded.
func testCoordinator(t *testing.T, ctx context.Context, st *PostgresStore,
	runner ProbeRunner, triggers TriggerSource) (*Coordinator, *[]time.Duration) {
	t.Helper()
	cfg := DefaultCoordinatorConfig("test:" + string(NewUUID()))
	cfg.TriggerInterval = 50 * time.Millisecond
	c, err := NewCoordinator(CoordinatorDeps{Store: st, Runner: runner,
		Triggers: triggers, Config: cfg, LogFn: func(string, string, ...any) {}})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	var mu sync.Mutex
	var slept []time.Duration
	c.sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		slept = append(slept, d)
		mu.Unlock()
		return ctx.Err()
	}
	if _, err := c.Bind(ctx); err != nil {
		t.Fatalf("bind: %v", err)
	}
	return c, &slept
}

func lockTrigger(subject string) Trigger {
	return Trigger{CaseID: "incident:db:lock_contention:" + subject,
		IncidentID: subject, Kind: TriggerLock, Subject: "incident " + subject,
		IdempotencyKey: "incident:" + subject}
}

func startAndRun(t *testing.T, ctx context.Context, c *Coordinator, tr Trigger) Investigation {
	t.Helper()
	inv, _, err := c.Start(ctx, tr)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := c.Investigate(ctx, inv.ID); err != nil {
		t.Fatalf("investigate: %v", err)
	}
	scope, _ := c.Scope()
	got, err := c.store.Get(ctx, scope, inv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return got
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
