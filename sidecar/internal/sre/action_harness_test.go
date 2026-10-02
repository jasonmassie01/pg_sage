package sre

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/store"
)

// Shared fixtures for the Sage SRE action tests: a concluded lock
// investigation whose root is an active backend (pid 5151), scripted
// target and recovery probes, a recording canceller and notifier, and
// the real approval queue on the test database.

const (
	testTargetPID = 5151
	testQueryHash = "c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00"
)

// activeChainRunner scripts an active root blocker (pid 5151) with an
// ALTER (pid 20) queued behind it and a reader (pid 30) behind the ALTER.
func activeChainRunner() *scriptedRunner {
	return newScriptedRunner().
		script(probes.LockGraph, activeDDLGraph()).
		script(probes.PreparedXacts, rows(probes.PreparedXacts))
}

// targetProbeRow is the signal_target row of the root blocker as it is
// right now; mutate changes what the fresh probe sees.
func targetProbeRow(mutate ...func(probes.Row)) probes.Result {
	r := probes.Row{"pid": int64(testTargetPID), "backend_start": deriveStart,
		"query_start": deriveStart.Add(time.Minute), "xact_start": deriveStart.Add(time.Minute),
		"datname": "orders", "usename": "app", "state": "active",
		"backend_type": "client backend", "waiting": false, "query_id": int64(77),
		"query_hash": testQueryHash, "blocking": int64(2), "in_recovery": false,
		"in_current_database": true, "privileged_role": false,
		"protected_application": false, "application_hash": strings.Repeat("d", 64)}
	for _, m := range mutate {
		m(r)
	}
	return rows(probes.SignalTarget, r)
}

// recoveryRows is a recovery_sample where the target is idle and the
// original waiters run.
func recoveryRows(targetBlocking bool) probes.Result {
	target := probes.Row{"pid": int64(testTargetPID), "backend_start": deriveStart,
		"state": "idle", "waiting": false, "blocked_by_target": false, "is_target": true}
	alter := probes.Row{"pid": int64(20), "backend_start": deriveStart.Add(20 * time.Second),
		"state": "active", "waiting": targetBlocking, "blocked_by_target": targetBlocking,
		"is_target": false}
	reader := probes.Row{"pid": int64(30), "backend_start": deriveStart.Add(30 * time.Second),
		"state": "active", "waiting": targetBlocking, "blocked_by_target": false,
		"is_target": false}
	if targetBlocking {
		target["state"] = "active"
	}
	return rows(probes.RecoverySample, target, alter, reader)
}

type fakeCanceller struct {
	mu       sync.Mutex
	calls    []executor.BackendCancel
	err      error
	actionID int64
	preview  executor.ActionPolicyDecision
	replicas []bool
	hook     func()
}

func newFakeCanceller() *fakeCanceller {
	return &fakeCanceller{actionID: 4711, preview: executor.ActionPolicyDecision{
		Decision: executor.PolicyDecisionExecute, RiskTier: "moderate"}}
}

func (f *fakeCanceller) CancelBackend(_ context.Context,
	req executor.BackendCancel) (int64, error) {
	f.mu.Lock()
	hook := f.hook
	f.calls = append(f.calls, req)
	err, id := f.err, f.actionID
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (f *fakeCanceller) PreviewBackendCancel(_ context.Context,
	isReplica bool) executor.ActionPolicyDecision {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replicas = append(f.replicas, isReplica)
	return f.preview
}

func (f *fakeCanceller) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type fakeNotifier struct {
	mu       sync.Mutex
	requests []ApprovalRequest
	err      error
}

func (n *fakeNotifier) ApprovalRequested(_ context.Context, r ApprovalRequest) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.requests = append(n.requests, r)
	return n.err
}

func (n *fakeNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.requests)
}

// testClock is a settable clock for recovery scheduling.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type actionHarness struct {
	st       *PostgresStore
	pool     *pgxpool.Pool
	ctx      context.Context
	coord    *Coordinator
	svc      *Service
	inv      Investigation
	targets  *scriptedRunner
	cancel   *fakeCanceller
	notes    *fakeNotifier
	queue    *PGApprovalQueue
	actions  *ActionService
	as       *store.ActionStore
	clock    *testClock
	cfg      ActionConfig
	diagnose *scriptedRunner
}

// newActionHarness concludes one lock investigation (scripted by
// diagnose, the active chain by default) and builds its action service.
func newActionHarness(t *testing.T, diagnose *scriptedRunner,
	mutate ...func(*ActionConfig)) *actionHarness {
	t.Helper()
	st, pool, ctx := liveStore(t, DefaultLimits())
	if diagnose == nil {
		diagnose = activeChainRunner()
	}
	c, _ := testCoordinator(t, ctx, st, diagnose, nil)
	h := &actionHarness{st: st, pool: pool, ctx: ctx, coord: c, diagnose: diagnose,
		svc: NewService("orders", c, st), cancel: newFakeCanceller(),
		notes: &fakeNotifier{}, queue: NewPGApprovalQueue(pool, nil),
		as: store.NewActionStore(pool), clock: &testClock{now: time.Now()},
		targets: newScriptedRunner().script(probes.SignalTarget, targetProbeRow()).
			script(probes.RecoverySample, recoveryRows(false))}
	h.inv = startAndRun(t, ctx, c, lockTrigger(string(NewUUID())))
	h.cfg = DefaultActionConfig()
	h.cfg.RequestApproval = false
	h.cfg.RecoveryInterval, h.cfg.RecoverySamples = time.Second, 3
	for _, m := range mutate {
		m(&h.cfg)
	}
	h.actions = h.newService(t, h.svc)
	return h
}

func (h *actionHarness) newService(t *testing.T, svc *Service) *ActionService {
	t.Helper()
	a, err := NewActionService(ActionDeps{Service: svc, Targets: h.targets,
		Queue: h.queue, Executor: h.cancel, Notifier: h.notes, Config: h.cfg,
		LogFn: func(string, string, ...any) {}, Now: h.clock.Now})
	if err != nil {
		t.Fatalf("NewActionService: %v", err)
	}
	return a
}

// requested proposes the investigation's cancel and queues its approval.
func (h *actionHarness) requested(t *testing.T) Proposal {
	t.Helper()
	p, err := h.actions.Propose(h.ctx, h.inv.ID, "user:1")
	if err != nil || p.State != ProposalProposed {
		t.Fatalf("propose = %+v, %v", p, err)
	}
	p, err = h.actions.RequestExecution(h.ctx, p.ID, "user:1")
	if err != nil || p.State != ProposalRequested || p.QueueID <= 0 {
		t.Fatalf("request = %+v, %v", p, err)
	}
	return p
}

// approved approves the proposal's queue item as user (single use).
func (h *actionHarness) approved(t *testing.T, p Proposal, user int) store.QueuedAction {
	t.Helper()
	a, err := h.as.Approve(h.ctx, p.QueueID, user)
	if err != nil {
		t.Fatalf("approve queue item %d: %v", p.QueueID, err)
	}
	return *a
}

func (h *actionHarness) proposal(t *testing.T, id UUID) Proposal {
	t.Helper()
	p, err := h.actions.Get(h.ctx, id)
	if err != nil {
		t.Fatalf("get proposal: %v", err)
	}
	return p
}

// eventTypes lists the investigation's event types in order.
func (h *actionHarness) eventTypes(t *testing.T) []string {
	t.Helper()
	scope, _ := h.coord.Scope()
	events, err := h.st.Events(h.ctx, scope, h.inv.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Type)
	}
	if err := h.st.VerifyEvents(h.ctx, scope, h.inv.ID); err != nil {
		t.Fatalf("event chain: %v", err)
	}
	return out
}

func countOf(types []string, want string) int {
	n := 0
	for _, t := range types {
		if t == want {
			n++
		}
	}
	return n
}

// queueRows counts approval items of a proposal, in any state.
func (h *actionHarness) queueRows(t *testing.T, id UUID) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(h.ctx, `SELECT count(*) FROM sage.action_queue
		WHERE identity_key = $1`, ApprovalIdentityPrefix+string(id)).Scan(&n); err != nil {
		t.Fatalf("count queue rows: %v", err)
	}
	return n
}

// targetWith is the target row with one column changed.
func targetWith(key string, value any) probes.Result {
	return targetProbeRow(func(r probes.Row) { r[key] = value })
}
