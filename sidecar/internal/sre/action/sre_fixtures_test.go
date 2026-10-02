package action

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Investigation fixtures built on the exported sre API: the action
// package cannot see package sre's test helpers, so the few it needs
// (scripted probes, a bound coordinator, a concluded lock investigation)
// live here.

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/sre/action"))
}

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
	r.mu.Lock()
	defer r.mu.Unlock()
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

func liveStore(t *testing.T, limits sre.Limits) (*sre.PostgresStore, *pgxpool.Pool,
	context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	st, err := sre.NewPostgresStore(pool, limits)
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	return st, pool, ctx
}

// testCoordinator builds a coordinator on a fresh scope whose compared
// samples do not wait.
func testCoordinator(t *testing.T, ctx context.Context, st *sre.PostgresStore,
	runner sre.ProbeRunner, triggers sre.TriggerSource) *sre.Coordinator {
	t.Helper()
	cfg := sre.DefaultCoordinatorConfig("test:" + string(sre.NewUUID()))
	cfg.TriggerInterval = 50 * time.Millisecond
	c, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: st, Runner: runner,
		Triggers: triggers, Config: cfg, LogFn: func(string, string, ...any) {},
		Wait: func(ctx context.Context, _ time.Duration) error { return ctx.Err() }})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	if _, err := c.Bind(ctx); err != nil {
		t.Fatalf("bind: %v", err)
	}
	return c
}

func lockTrigger(subject string) sre.Trigger {
	return sre.Trigger{CaseID: "incident:db:lock_contention:" + subject,
		IncidentID: subject, Kind: sre.TriggerLock, Subject: "incident " + subject,
		IdempotencyKey: "incident:" + subject}
}

func startAndRun(t *testing.T, ctx context.Context, st *sre.PostgresStore,
	c *sre.Coordinator, tr sre.Trigger) sre.Investigation {
	t.Helper()
	inv, _, err := c.Start(ctx, tr)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := c.Investigate(ctx, inv.ID); err != nil {
		t.Fatalf("investigate: %v", err)
	}
	scope, _ := c.Scope()
	got, err := st.Get(ctx, scope, inv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return got
}
