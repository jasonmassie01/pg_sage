package sre

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Leases and fencing (Codex §6, CHECK-14/24): one worker holds an
// investigation at a time; a stale worker cannot commit; stop, resume
// and restart preserve steps, evidence and consumed budget.

func probeResult(id probes.ID, rows ...probes.Row) probes.Result {
	st := probes.StatusOK
	if len(rows) == 0 {
		st = probes.StatusEmpty
	}
	return probes.Result{ProbeID: id, Version: "v1", Status: st,
		Columns: []string{"pid"}, Rows: rows, ObservedAt: time.Now()}
}

func step(key string, next State, results ...probes.Result) StepResult {
	return StepResult{IdempotencyKey: key, NextState: next, Results: results}
}

func TestStore_ClaimRaceHasExactlyOneWinner(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	inv, _, err := st.Create(ctx, lockStart(testScope(t, ctx, st), "pid 20"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	const n = 8
	var wins, lost int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrLeaseUnavailable):
				lost++
			default:
				t.Errorf("claim: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 || lost != n-1 {
		t.Fatalf("wins=%d lost=%d, want exactly one lease", wins, lost)
	}
	got, _ := st.Get(ctx, inv.Scope, inv.ID)
	if got.State != StateCollecting || got.Fence != 1 {
		t.Fatalf("after claim = %+v, want collecting with fence 1", got)
	}
}

// CHECK-14: two workers and an expired lease cannot commit conflicting
// step results.
func TestStore_StaleWorkerCannotCommit(t *testing.T) {
	limits := DefaultLimits()
	limits.LeaseTTL = 300 * time.Millisecond
	st, _, ctx := liveStore(t, limits)
	inv, _, _ := st.Create(ctx, lockStart(testScope(t, ctx, st), "pid 21"))
	a, err := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
	if err != nil {
		t.Fatalf("claim A: %v", err)
	}
	if _, err := st.Claim(ctx, inv.Scope, inv.ID, NewUUID()); !errors.Is(err,
		ErrLeaseUnavailable) {
		t.Fatalf("claim while A holds the lease = %v", err)
	}
	time.Sleep(800 * time.Millisecond)
	b, err := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
	if err != nil || b.Fence != a.Fence+1 {
		t.Fatalf("claim B after expiry = %+v (%v), want fence %d", b, err, a.Fence+1)
	}
	_, err = st.CommitStep(ctx, a, step("a-1", StateCollecting,
		probeResult(probes.LockGraph, probes.Row{"pid": int64(1)})))
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale worker commit = %v, want ErrLeaseLost", err)
	}
	got, err := st.CommitStep(ctx, b, step("b-1", StateCollecting,
		probeResult(probes.LockGraph, probes.Row{"pid": int64(2)})))
	if err != nil || got.ProbeCount != 1 {
		t.Fatalf("current worker commit = %+v (%v)", got, err)
	}
	ev, err := st.Evidence(ctx, inv.Scope, inv.ID)
	if err != nil || len(ev) != 1 || ev[0].StepKey != "b-1" {
		t.Fatalf("evidence = %+v (%v), want only worker B's", ev, err)
	}
}

func TestStore_ExpiredLeaseCannotCommitEvenWithoutRival(t *testing.T) {
	limits := DefaultLimits()
	limits.LeaseTTL = 200 * time.Millisecond
	st, _, ctx := liveStore(t, limits)
	inv, _, _ := st.Create(ctx, lockStart(testScope(t, ctx, st), "pid 22"))
	a, _ := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
	time.Sleep(800 * time.Millisecond)
	if _, err := st.CommitStep(ctx, a, step("late", StateCollecting)); !errors.Is(err,
		ErrLeaseLost) {
		t.Fatalf("commit after lease expiry = %v, want ErrLeaseLost", err)
	}
	if _, err := st.Heartbeat(ctx, a); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("heartbeat after expiry = %v, want ErrLeaseLost", err)
	}
}

func TestStore_HeartbeatExtendsWithinSegment(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	inv, _, _ := st.Create(ctx, lockStart(testScope(t, ctx, st), "pid 23"))
	a, err := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	b, err := st.Heartbeat(ctx, a)
	if err != nil || !b.Until.After(a.Until) || b.Until.After(b.SegmentDeadline) {
		t.Fatalf("heartbeat = %+v (%v), want a later lease within the segment", b, err)
	}
	var stored time.Time
	if err := pool.QueryRow(ctx, `SELECT lease_until FROM sage.sre_investigations
		WHERE id = $1`, string(inv.ID)).Scan(&stored); err != nil ||
		!stored.Equal(b.Until) {
		t.Fatalf("stored lease_until %v, want the extension %v (%v)", stored, b.Until, err)
	}
	if _, err := st.CommitStep(ctx, b, step("hb", StateCollecting)); err != nil {
		t.Fatalf("commit on the extended lease: %v", err)
	}
}

func TestStore_CommitStepIsIdempotent(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	inv, _, _ := st.Create(ctx, lockStart(testScope(t, ctx, st), "pid 24"))
	lease, _ := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
	res := probeResult(probes.LongTransactions, probes.Row{"pid": int64(5)})
	first, err := st.CommitStep(ctx, lease, step("same", StateCollecting, res))
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	second, err := st.CommitStep(ctx, lease, step("same", StateCollecting, res))
	if err != nil || second.ProbeCount != first.ProbeCount {
		t.Fatalf("repeated commit = %+v (%v), probe count %d -> %d", second, err,
			first.ProbeCount, second.ProbeCount)
	}
	if ev, _ := st.Evidence(ctx, inv.Scope, inv.ID); len(ev) != 1 {
		t.Fatalf("evidence rows = %d, want 1", len(ev))
	}
}

// Boundary: the probe cap is a hard ceiling per investigation.
func TestStore_ProbeCapBoundary(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxProbes = 3
	st, _, ctx := liveStore(t, limits)
	inv, _, _ := st.Create(ctx, lockStart(testScope(t, ctx, st), "pid 25"))
	lease, _ := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
	r := probeResult(probes.LockGraph)
	if got, err := st.CommitStep(ctx, lease, step("two", StateCollecting, r, r)); err != nil ||
		got.ProbeCount != 2 {
		t.Fatalf("2 probes = %+v (%v)", got, err)
	}
	if got, err := st.CommitStep(ctx, lease, step("third", StateCollecting, r)); err != nil ||
		got.ProbeCount != 3 {
		t.Fatalf("3rd probe = %+v (%v)", got, err)
	}
	if _, err := st.CommitStep(ctx, lease, step("fourth", StateCollecting, r)); !errors.Is(
		err, ErrBudgetExhausted) {
		t.Fatalf("4th probe = %v, want ErrBudgetExhausted", err)
	}
	got, _ := st.Get(ctx, inv.Scope, inv.ID)
	if got.ProbeCount != 3 {
		t.Fatalf("probe count = %d after a refused step, want 3", got.ProbeCount)
	}
}

func TestStore_EvidenceIsHashedAndTyped(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	inv, _, _ := st.Create(ctx, lockStart(testScope(t, ctx, st), "pid 26"))
	lease, _ := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
	denied := probes.Result{ProbeID: probes.PlanRegressions, Version: "v1",
		Status: probes.StatusNoPrivilege, Reason: "insufficient_privilege",
		ObservedAt: time.Now()}
	ok := probeResult(probes.LockGraph, probes.Row{"pid": int64(9)})
	if _, err := st.CommitStep(ctx, lease, step("mixed", StateCollecting, ok,
		denied)); err != nil {
		t.Fatalf("commit: %v", err)
	}
	ev, err := st.Evidence(ctx, inv.Scope, inv.ID)
	if err != nil || len(ev) != 2 {
		t.Fatalf("evidence = %+v (%v)", ev, err)
	}
	byProbe := map[string]Evidence{}
	for _, e := range ev {
		byProbe[e.ProbeID] = e
		if len(e.SHA256) != 32 || !e.VerifyHash() {
			t.Fatalf("evidence %s hash invalid", e.ID)
		}
	}
	if byProbe["lock_graph"].CapabilityState != "available" ||
		byProbe["plan_regressions"].CapabilityState != "permission_denied" ||
		byProbe["plan_regressions"].ReasonCode != "insufficient_privilege" {
		t.Fatalf("capability states = %+v", byProbe)
	}
}

// State transitions of an investigation through the store.
func TestStore_StateTransitions(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	inv, _, _ := st.Create(ctx, lockStart(testScope(t, ctx, st), "pid 27"))
	lease, _ := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
	if _, err := st.CommitStep(ctx, lease, step("bad", StateConcluded)); !errors.Is(err,
		ErrInvalidTransition) {
		t.Fatalf("collecting -> concluded = %v, want ErrInvalidTransition", err)
	}
	got, err := st.CommitStep(ctx, lease, step("eval", StateEvaluating))
	if err != nil || got.State != StateEvaluating {
		t.Fatalf("collecting -> evaluating = %+v (%v)", got, err)
	}
	got, err = st.Release(ctx, lease, StateNeedsEvidence)
	if err != nil || got.State != StateNeedsEvidence || got.LeaseOwner != "" {
		t.Fatalf("evaluating -> needs_evidence = %+v (%v)", got, err)
	}
	lease2, err := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
	if err != nil {
		t.Fatalf("re-claim needs_evidence: %v", err)
	}
	if _, err := st.CommitStep(ctx, lease2, step("eval2", StateEvaluating)); err != nil {
		t.Fatalf("collecting -> evaluating: %v", err)
	}
	done, err := st.Release(ctx, lease2, StateInconclusive)
	if err != nil || done.State != StateInconclusive {
		t.Fatalf("evaluating -> inconclusive = %+v (%v)", done, err)
	}
	if _, err := st.Claim(ctx, inv.Scope, inv.ID, NewUUID()); !errors.Is(err, ErrTerminal) {
		t.Fatalf("claim terminal = %v, want ErrTerminal", err)
	}
	if _, err := st.Stop(ctx, inv.Scope, inv.ID, done.Version); !errors.Is(err,
		ErrInvalidTransition) {
		t.Fatalf("stop terminal = %v, want ErrInvalidTransition", err)
	}
	if _, err := st.Resume(ctx, inv.Scope, inv.ID, done.Version); !errors.Is(err,
		ErrInvalidTransition) {
		t.Fatalf("resume terminal = %v, want ErrInvalidTransition", err)
	}
}

func TestStore_OperatorVersionPreconditions(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	inv, _, _ := st.Create(ctx, lockStart(testScope(t, ctx, st), "pid 28"))
	if _, err := st.Pause(ctx, inv.Scope, inv.ID, inv.Version+5); !errors.Is(err,
		ErrVersionConflict) {
		t.Fatalf("pause with a stale version = %v, want ErrVersionConflict", err)
	}
	if _, err := st.Resume(ctx, inv.Scope, inv.ID, inv.Version); !errors.Is(err,
		ErrInvalidTransition) {
		t.Fatalf("resume queued = %v, want ErrInvalidTransition", err)
	}
	if _, err := st.Pause(ctx, inv.Scope, NewUUID(), 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pause unknown id = %v, want ErrNotFound", err)
	}
}

// CHECK-24: stop/resume/restart preserve steps, evidence and consumed
// budget; a paused investigation keeps its history and a stale lease
// from before the pause cannot commit.
func TestStore_PauseResumeRestartPreservesState(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	inv, _, _ := st.Create(ctx, lockStart(testScope(t, ctx, st), "pid 29"))
	lease, _ := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
	if _, err := st.CommitStep(ctx, lease, step("s1", StateCollecting,
		probeResult(probes.LockGraph, probes.Row{"pid": int64(1)}))); err != nil {
		t.Fatalf("commit: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	cur, _ := st.Get(ctx, inv.Scope, inv.ID)
	paused, err := st.Pause(ctx, inv.Scope, inv.ID, cur.Version)
	if err != nil || paused.State != StatePaused || paused.LeaseOwner != "" ||
		paused.ActiveMS < 200 {
		t.Fatalf("pause = %+v (%v), want paused, lease cleared, time charged", paused, err)
	}
	if _, err := st.CommitStep(ctx, lease, step("s-stale", StateCollecting)); !errors.Is(
		err, ErrLeaseLost) {
		t.Fatalf("commit after pause = %v, want ErrLeaseLost", err)
	}

	restarted, err := NewPostgresStore(pool, DefaultLimits())
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	resumed, err := restarted.Resume(ctx, inv.Scope, inv.ID, paused.Version)
	if err != nil || resumed.State != StateQueued || resumed.ProbeCount != 1 ||
		resumed.ActiveMS != paused.ActiveMS || resumed.Version <= paused.Version {
		t.Fatalf("resume after restart = %+v (%v)", resumed, err)
	}
	lease2, err := restarted.Claim(ctx, inv.Scope, inv.ID, NewUUID())
	if err != nil {
		t.Fatalf("claim after resume: %v", err)
	}
	if _, err := restarted.CommitStep(ctx, lease2, step("s2", StateCollecting,
		probeResult(probes.LongTransactions))); err != nil {
		t.Fatalf("commit after resume: %v", err)
	}
	ev, _ := restarted.Evidence(ctx, inv.Scope, inv.ID)
	if len(ev) != 2 {
		t.Fatalf("evidence after pause/restart/resume = %d rows, want 2", len(ev))
	}
	cur, _ = restarted.Get(ctx, inv.Scope, inv.ID)
	stopped, err := restarted.Stop(ctx, inv.Scope, inv.ID, cur.Version)
	if err != nil || stopped.State != StateCancelled || stopped.ProbeCount != 2 {
		t.Fatalf("stop = %+v (%v)", stopped, err)
	}
}

// Active time is charged on release and, for an orphaned lease, the
// whole reserved segment is charged before another worker resumes. Two
// stores share the database: one with short leases (workers that die)
// and one with long leases (a worker that finishes its step). Sleeps
// only ever wait for a lease to expire, which tolerates the database
// clock running ahead of the host's.
func TestStore_ActiveTimeIsChargedAndCapped(t *testing.T) {
	short := DefaultLimits()
	short.MaxActive, short.LeaseTTL = 3*time.Second, 300*time.Millisecond
	dying, pool, ctx := liveStore(t, short)
	long := short
	long.LeaseTTL = 3 * time.Second
	st, err := NewPostgresStore(pool, long)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	inv, _, _ := st.Create(ctx, lockStart(testScope(t, ctx, st), "pid 30"))
	if _, err := dying.Claim(ctx, inv.Scope, inv.ID, NewUUID()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	time.Sleep(800 * time.Millisecond) // the worker dies holding its lease
	b, err := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
	if err != nil {
		t.Fatalf("claim after orphan: %v", err)
	}
	if got, _ := st.Get(ctx, inv.Scope, inv.ID); got.ActiveMS != 300 {
		t.Fatalf("orphaned segment charged %d ms, want the whole 300 ms lease",
			got.ActiveMS)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := st.CommitStep(ctx, b, step("eval", StateEvaluating)); err != nil {
		t.Fatalf("collecting -> evaluating: %v", err)
	}
	rel, err := st.Release(ctx, b, StateNeedsEvidence)
	if err != nil || rel.ActiveMS < 500 || rel.ActiveMS >= 3000 {
		t.Fatalf("release charged %d ms (%v), want 500..2999", rel.ActiveMS, err)
	}
	c, err := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
	if err != nil || c.Until.After(c.SegmentDeadline) {
		t.Fatalf("claim with time left = %+v (%v)", c, err)
	}
	// This worker dies too; its lease is at most the remaining budget.
	time.Sleep(time.Duration(3000-rel.ActiveMS)*time.Millisecond + 800*time.Millisecond)
	if _, err := st.Claim(ctx, inv.Scope, inv.ID, NewUUID()); !errors.Is(err,
		ErrBudgetExhausted) {
		t.Fatalf("claim past the active-time budget = %v, want ErrBudgetExhausted", err)
	}
	final, _ := st.Get(ctx, inv.Scope, inv.ID)
	if final.ActiveMS != short.MaxActive.Milliseconds() || final.LeaseOwner != "" {
		t.Fatalf("final = %d ms owner %q, want exactly the cap and no lease",
			final.ActiveMS, final.LeaseOwner)
	}
}

func TestStore_UnknownInvestigationAndScope(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	if _, err := st.Get(ctx, scope, NewUUID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get unknown = %v", err)
	}
	if _, err := st.Claim(ctx, scope, NewUUID(), NewUUID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Claim unknown = %v", err)
	}
	if _, err := st.Get(ctx, Scope{}, NewUUID()); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Get with invalid scope = %v", err)
	}
	var zero Lease
	if _, err := st.CommitStep(ctx, zero, step("x", StateCollecting)); !errors.Is(err,
		ErrInvalidRequest) {
		t.Fatalf("CommitStep with a zero lease = %v", err)
	}
}
