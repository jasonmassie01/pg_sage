package sre

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Stale resume (sre-02): a run resumed long after its first step (a
// sidecar restart picking up an orphaned row, an operator resuming a
// paused one) must not conclude on that evidence as if it were current.
// It ends expired with failure code stale_evidence, by the system, on
// the event chain, without running a probe or the model.

// agedLockStep is the lock plan's one step (the idle-in-transaction
// chain) observed at at.
func agedLockStep(at time.Time, next State) StepResult {
	seed := idleChainRunner()
	var rs []probes.Result
	for _, id := range []probes.ID{probes.LockGraph, probes.PreparedXacts,
		probes.LongTransactions, probes.SageActions} {
		r := seed.Run(context.Background(), id, probes.Args{})
		r.ObservedAt = at
		rs = append(rs, r)
	}
	return step("step-1", next, rs...)
}

// orphanLockRun starts a lock investigation whose worker committed its
// step with evidence observed at at and then died (its lease expired).
func orphanLockRun(t *testing.T, ctx context.Context, st *PostgresStore, pool *pgxpool.Pool,
	c *Coordinator, subject string, at time.Time) Investigation {
	t.Helper()
	scope, _ := c.Scope()
	inv, _, err := c.Start(ctx, lockTrigger(subject))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	dead, err := st.Claim(ctx, scope, inv.ID, NewUUID())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := st.CommitStep(ctx, dead, agedLockStep(at, StateEvaluating)); err != nil {
		t.Fatalf("dead worker step: %v", err)
	}
	expireLease(t, ctx, pool, dead)
	return inv
}

// staleExpiries counts the system's transitions to expired for stale
// evidence; concluded counts conclusion events.
func staleExpiries(t *testing.T, ctx context.Context, st *PostgresStore, scope Scope,
	id UUID) (stale, concluded int) {
	t.Helper()
	events, err := st.Events(ctx, scope, id)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, e := range events {
		var p struct{ To, Reason string }
		_ = json.Unmarshal(e.Payload, &p)
		switch {
		case e.Type == EventConcluded:
			concluded++
		case e.Type == EventTransition && e.Actor == "system" && p.To == "expired" &&
			p.Reason == ReasonStaleEvidence:
			stale++
		}
	}
	return stale, concluded
}

// assertStaleExpired checks the run ended expired for stale evidence,
// with no hypotheses, the evidence age recorded and one system event.
func assertStaleExpired(t *testing.T, ctx context.Context, st *PostgresStore, scope Scope,
	id UUID, minAge time.Duration) {
	t.Helper()
	got, err := st.Get(ctx, scope, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != StateExpired || got.FailureCode != ReasonStaleEvidence ||
		got.Summary.Reason != ReasonStaleEvidence || got.Summary.Root != "" ||
		got.LeaseOwner != "" || got.ConcludedAt.IsZero() {
		t.Fatalf("stale resume = state %s, failure %q, summary %+v, lease %q", got.State,
			got.FailureCode, got.Summary, got.LeaseOwner)
	}
	if got.Summary.EvidenceNewestAt == nil ||
		got.Summary.EvidenceAgeS < int64(minAge/time.Second) ||
		got.Summary.EvidenceAgeS > int64((minAge+5*time.Minute)/time.Second) {
		t.Fatalf("evidence age = newest %v, %d s; want at least %s",
			got.Summary.EvidenceNewestAt, got.Summary.EvidenceAgeS, minAge)
	}
	if hs, _ := st.Hypotheses(ctx, scope, id); len(hs) != 0 {
		t.Fatalf("a stale run wrote %d hypotheses", len(hs))
	}
	if stale, concluded := staleExpiries(t, ctx, st, scope, id); stale != 1 || concluded != 0 {
		t.Fatalf("events: %d stale expiries, %d conclusions; want 1 and 0", stale, concluded)
	}
}

// State transition: an orphaned run whose evidence is an hour old ends
// expired instead of concluding idle_in_tx_holder on it.
func TestStaleResume_OrphanedRunExpiresWithoutProbing(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	runner := idleChainRunner()
	c, _ := testCoordinator(t, ctx, st, runner, nil)
	scope, _ := c.Scope()
	inv := orphanLockRun(t, ctx, st, pool, c, "stale-1", time.Now().Add(-time.Hour))
	if err := c.Investigate(ctx, inv.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	assertStaleExpired(t, ctx, st, scope, inv.ID, time.Hour)
	if runner.total() != 0 {
		t.Fatalf("a stale resume ran %d probes", runner.total())
	}
	if err := c.Investigate(ctx, inv.ID); err != nil || runner.total() != 0 {
		t.Fatalf("re-investigating an expired run = %v with %d probes", err, runner.total())
	}
}

// State transition: paused, then resumed an hour later (the coordinator
// clock moves; the stored evidence cannot be rewritten).
func TestStaleResume_PausedRunResumedAnHourLaterExpires(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	runner := idleChainRunner()
	c, _ := testCoordinator(t, ctx, st, runner, nil)
	scope, _ := c.Scope()
	inv, _, err := c.Start(ctx, lockTrigger("stale-2"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	dead, err := st.Claim(ctx, scope, inv.ID, NewUUID())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := st.CommitStep(ctx, dead,
		agedLockStep(time.Now(), StateCollecting)); err != nil {
		t.Fatalf("step: %v", err)
	}
	cur, _ := st.Get(ctx, scope, inv.ID)
	paused, err := st.Pause(ctx, scope, inv.ID, cur.Version)
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := st.Resume(ctx, scope, inv.ID, paused.Version); err != nil {
		t.Fatalf("resume: %v", err)
	}
	c.now = func() time.Time { return time.Now().Add(time.Hour) }
	if err := c.Investigate(ctx, inv.ID); err != nil {
		t.Fatalf("investigate: %v", err)
	}
	assertStaleExpired(t, ctx, st, scope, inv.ID, time.Hour-time.Minute)
	if runner.total() != 0 {
		t.Fatalf("a stale resume ran %d probes", runner.total())
	}
}

// Boundary on the real store: evidence exactly MaxEvidenceSpread old at
// resume concludes; one second more expires.
func TestStaleResume_SpreadBoundary(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	runner := idleChainRunner()
	c, _ := testCoordinator(t, ctx, st, runner, nil)
	scope, _ := c.Scope()
	observed := time.Now().UTC().Truncate(time.Second)
	within := orphanLockRun(t, ctx, st, pool, c, "stale-3", observed)
	c.now = func() time.Time { return observed.Add(MaxEvidenceSpread) }
	if err := c.Investigate(ctx, within.ID); err != nil {
		t.Fatalf("resume within the spread: %v", err)
	}
	got, _ := st.Get(ctx, scope, within.ID)
	if got.State != StateConcluded || got.Summary.Root != "idle_in_tx_holder" ||
		got.Summary.EvidenceAgeS != int64(MaxEvidenceSpread/time.Second) ||
		got.Summary.EvidenceNewestAt == nil || !got.Summary.EvidenceNewestAt.Equal(observed) {
		t.Fatalf("evidence exactly %s old = %s %q age %d newest %v", MaxEvidenceSpread,
			got.State, got.Summary.Root, got.Summary.EvidenceAgeS, got.Summary.EvidenceNewestAt)
	}
	beyond := orphanLockRun(t, ctx, st, pool, c, "stale-4", observed)
	c.now = func() time.Time { return observed.Add(MaxEvidenceSpread + time.Second) }
	if err := c.Investigate(ctx, beyond.ID); err != nil {
		t.Fatalf("resume past the spread: %v", err)
	}
	assertStaleExpired(t, ctx, st, scope, beyond.ID, MaxEvidenceSpread+time.Second)
	if runner.total() != 0 {
		t.Fatalf("resumed runs ran %d probes; the plan's step was committed", runner.total())
	}
}

// Happy path: an immediate resume still skips the committed step and
// concludes, with the evidence age on the summary.
func TestStaleResume_ImmediateResumeConcludes(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	runner := leakRunner()
	c, _ := testCoordinator(t, ctx, st, runner, nil)
	scope, _ := c.Scope()
	inv, _, err := c.Start(ctx, Trigger{CaseID: "case:conn", Kind: TriggerConnections,
		Subject: "incident 11", IdempotencyKey: "incident:11"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	dead, err := st.Claim(ctx, scope, inv.ID, NewUUID())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := st.CommitStep(ctx, dead, step("step-1", StateCollecting,
		runner.Run(ctx, probes.ConnectionSaturation, probes.Args{}),
		runner.Run(ctx, probes.LockGraph, probes.Args{}))); err != nil {
		t.Fatalf("dead worker step: %v", err)
	}
	expireLease(t, ctx, pool, dead)
	if err := c.Investigate(ctx, inv.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	got, _ := st.Get(ctx, scope, inv.ID)
	if got.State != StateConcluded || got.Summary.Root != "connection_leak" {
		t.Fatalf("immediate resume = %s %q", got.State, got.Summary.Root)
	}
	if n := runner.calls[probes.ConnectionSaturation]; n != 2 {
		t.Fatalf("connection_saturation ran %d times, want 2", n)
	}
	if got.Summary.EvidenceNewestAt == nil || got.Summary.EvidenceAgeS < 0 ||
		got.Summary.EvidenceAgeS > 60 {
		t.Fatalf("evidence age = newest %v, %d s; want a recent age",
			got.Summary.EvidenceNewestAt, got.Summary.EvidenceAgeS)
	}
}

// Nil/empty and CHECK-02: a fresh run has no stored evidence when it is
// claimed, is unaffected, and every conclusion (conclusive or not)
// records the age of its newest evidence.
func TestStaleResume_FreshRunsRecordEvidenceAge(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	c, _ := testCoordinator(t, ctx, st, idleChainRunner(), nil)
	quiet, _ := testCoordinator(t, ctx, st, newScriptedRunner(), nil)
	lock := startAndRun(t, ctx, c, lockTrigger("fresh-1"))
	if lock.State != StateConcluded || lock.Summary.Root != "idle_in_tx_holder" {
		t.Fatalf("fresh lock run = %s %q", lock.State, lock.Summary.Root)
	}
	idle := startAndRun(t, ctx, quiet, lockTrigger("fresh-2"))
	if idle.State != StateInconclusive {
		t.Fatalf("fresh run without lock waits = %s (%s)", idle.State, idle.FailureCode)
	}
	for _, got := range []Investigation{lock, idle} {
		if got.Summary.EvidenceNewestAt == nil || got.Summary.EvidenceAgeS > 60 ||
			time.Since(*got.Summary.EvidenceNewestAt) > time.Minute {
			t.Fatalf("%s summary lacks a current evidence age: newest %v, %d s", got.State,
				got.Summary.EvidenceNewestAt, got.Summary.EvidenceAgeS)
		}
	}
}

// Concurrent access: two workers resume the same stale orphan at once;
// it expires exactly once and neither runs a probe.
func TestStaleResume_ConcurrentResumesExpireOnce(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	runner := idleChainRunner()
	a, _ := testCoordinator(t, ctx, st, runner, nil)
	scope, _ := a.Scope()
	inv := orphanLockRun(t, ctx, st, pool, a, "stale-5", time.Now().Add(-time.Hour))
	b, err := NewCoordinator(CoordinatorDeps{Store: st, Runner: runner,
		Config: a.cfg, LogFn: func(string, string, ...any) {}})
	if err != nil {
		t.Fatalf("second coordinator: %v", err)
	}
	if _, err := b.Bind(ctx); err != nil {
		t.Fatalf("bind: %v", err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, c := range []*Coordinator{a, b} {
		wg.Add(1)
		go func(i int, c *Coordinator) {
			defer wg.Done()
			errs[i] = c.Investigate(ctx, inv.ID)
		}(i, c)
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatalf("concurrent resumes: %v", err)
	}
	assertStaleExpired(t, ctx, st, scope, inv.ID, time.Hour)
	if runner.total() != 0 {
		t.Fatalf("concurrent stale resumes ran %d probes", runner.total())
	}
}

// Error propagation: ExpireStale is fenced. A lost lease is ErrLeaseLost
// and leaves the run untouched; a malformed lease is ErrInvalidRequest.
func TestStore_ExpireStaleIsFenced(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	inv, _, err := st.Create(ctx, lockStart(testScope(t, ctx, st), "pid 77"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	lease, err := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	expireLease(t, ctx, pool, lease)
	sum := Summary{Reason: ReasonStaleEvidence}
	if _, err := st.ExpireStale(ctx, lease, sum); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expire on a lost lease = %v, want ErrLeaseLost", err)
	}
	got, _ := st.Get(ctx, inv.Scope, inv.ID)
	if got.State != StateCollecting || got.FailureCode != "" {
		t.Fatalf("a refused expiry changed the run: %s %q", got.State, got.FailureCode)
	}
	if _, err := st.ExpireStale(ctx, Lease{}, sum); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expire with an empty lease = %v, want ErrInvalidRequest", err)
	}
}

// The age on a conclusion counts the evidence the model turn added: a
// model probe observed after the plan's steps is the newest evidence, so
// the age is not overstated by the plan's older rows.
func TestStaleResume_AgeCountsTheModelProbe(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	m := newFakeModel(t, probeFirstReview(t), toolReply(validIdleReview(t)))
	runner := retryLockRunner()
	runner.hook = func(id probes.ID, call int) {
		if id == probes.LockGraph && call == 1 { // the model's probe
			time.Sleep(50 * time.Millisecond)
		}
	}
	c, _ := modelCoordinator(t, ctx, st, runner, m.client())
	inv := startAndRun(t, ctx, c, lockTrigger("stale-model-probe"))
	assertIdleRoot(t, st, inv)
	ev, err := st.Evidence(ctx, inv.Scope, inv.ID)
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	var probed, planned time.Time
	for _, e := range ev {
		if strings.HasPrefix(e.StepKey, "model-probe") {
			probed = e.ObservedAt
		} else if e.ObservedAt.After(planned) {
			planned = e.ObservedAt
		}
	}
	if probed.IsZero() || !probed.After(planned) {
		t.Fatalf("model probe at %v, plan steps at %v; want a later model probe", probed,
			planned)
	}
	got := inv.Summary.EvidenceNewestAt
	if got == nil || !got.Equal(probed) {
		t.Fatalf("evidence_newest_at = %v, want the model probe's %v (plan steps %v)", got,
			probed, planned)
	}
}
