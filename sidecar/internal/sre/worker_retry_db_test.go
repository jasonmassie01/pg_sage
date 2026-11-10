package sre

import (
	"context"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// sre-06 on the real store: a probe refused its slot once is retried and
// the investigation concludes from usable evidence; the investigator's
// read is retried the same way and still commits one step through
// needs_evidence -> collecting -> evaluating.

func TestCoordinator_QueueRefusedProbeIsRetriedOnce(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	runner := idleChainRunner()
	graph := runner.scripts[probes.LockGraph][0]
	runner.script(probes.LockGraph, queueRefusal(probes.LockGraph), graph)
	c, _ := testCoordinator(t, ctx, st, runner, nil)
	inv := startAndRun(t, ctx, c, lockTrigger("queue-retry"))
	if inv.State != StateConcluded || inv.Summary.Root != "idle_in_tx_holder" ||
		inv.ProbeCount != 4 {
		t.Fatalf("investigation = %+v, want the idle holder from 4 committed results", inv)
	}
	if runner.total() != 5 {
		t.Fatalf("probe runs = %d, want 5 (4 plus one retry)", runner.total())
	}
	ev, err := st.Evidence(ctx, inv.Scope, inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ev {
		if e.ReasonCode == "concurrency_limit" {
			t.Fatalf("evidence %s kept the refused attempt: %+v", e.ProbeID, e)
		}
	}
	if err := st.VerifyEvents(ctx, inv.Scope, inv.ID); err != nil {
		t.Fatalf("event chain: %v", err)
	}
}

// evaluatingSession is an investigator session on a claimed
// investigation in evaluating, as the agent loop holds it.
func evaluatingSession(t *testing.T, ctx context.Context, st *PostgresStore,
	subject string) *investigatorSession {
	t.Helper()
	c, err := NewCoordinator(CoordinatorDeps{Store: st, Runner: newScriptedRunner(),
		Config: DefaultCoordinatorConfig("retry:" + string(NewUUID()))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Bind(ctx); err != nil {
		t.Fatal(err)
	}
	inv, _, err := c.Start(ctx, lockTrigger(subject))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := st.Claim(ctx, inv.Scope, inv.ID, c.worker)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []State{StateCollecting, StateEvaluating} {
		if _, err := st.CommitStep(ctx, lease, StepResult{IdempotencyKey: string(state),
			NextState: state}); err != nil {
			t.Fatal(err)
		}
	}
	return &investigatorSession{c: c, inv: inv, lease: lease}
}

func TestCollect_QueueRefusalIsRetriedAndCommittedOnce(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	s := evaluatingSession(t, ctx, st, "collect-retry")
	reads := 0
	out, err := s.collect(ctx, func(context.Context) probes.Result {
		reads++
		if reads == 1 {
			return queueRefusal(probes.LongTransactions)
		}
		return rows(probes.LongTransactions, probes.Row{"pid": int64(4242)})
	})
	if err != nil || out.Status != string(probes.StatusOK) || out.Evidence == nil {
		t.Fatalf("collect = %+v, err %v, want the retried usable read", out, err)
	}
	if reads != 2 {
		t.Fatalf("reads = %d, want 2 (one retry)", reads)
	}
	ev, err := st.Evidence(ctx, s.inv.Scope, s.inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 1 || ev[0].ProbeID != string(probes.LongTransactions) ||
		ev[0].ReasonCode == "concurrency_limit" {
		t.Fatalf("evidence = %+v, want the one usable read", ev)
	}
	got, err := st.Get(ctx, s.inv.Scope, s.inv.ID)
	if err != nil || got.State != StateEvaluating {
		t.Fatalf("state = %s (err %v), want evaluating after one commit", got.State, err)
	}
	if s.commits != 1 {
		t.Fatalf("commits = %d, want one needs_evidence -> evaluating cycle", s.commits)
	}
}

func TestCollect_TwoQueueRefusalsCommitConcurrencyLimit(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	s := evaluatingSession(t, ctx, st, "collect-refused")
	reads := 0
	out, err := s.collect(ctx, func(context.Context) probes.Result {
		reads++
		return queueRefusal(probes.LongTransactions)
	})
	if err != nil || out.Status != string(probes.StatusError) || out.Evidence == nil {
		t.Fatalf("collect = %+v, err %v, want the refusal committed", out, err)
	}
	ev, err := st.Evidence(ctx, s.inv.Scope, s.inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reads != 2 || len(ev) != 1 || ev[0].ReasonCode != "concurrency_limit" {
		t.Fatalf("reads %d evidence %+v, want 2 reads and one concurrency_limit", reads, ev)
	}
}

func TestRunbookRun_QueueRefusedProbeIsRetriedOnce(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	runner := idleChainRunner().script(probes.LockChains, queueRefusal(probes.LockChains),
		probes.Result{ProbeID: probes.LockChains, Version: "v1", Status: probes.StatusEmpty,
			Reason: "no_rows"})
	inv, _ := runIdle(t, ctx, st, runner, func(scope Scope) {
		signedRunbook(t, ctx, st, scope, idleRunbook())
	})
	if inv.State != StateConcluded || inv.ProbeCount != 5 {
		t.Fatalf("investigation %s with %d probes, want concluded with 5", inv.State,
			inv.ProbeCount)
	}
	ev := runbookEvidence(t, st, inv)
	if len(ev) != 1 || ev[0].ProbeID != string(probes.LockChains) ||
		ev[0].ReasonCode == "concurrency_limit" {
		t.Fatalf("runbook evidence = %+v, want the retried lock_chains read", ev)
	}
	if run := inv.Summary.Runbook; run == nil || run.Outcome != RunbookCompleted {
		t.Fatalf("summary runbook = %+v, want a completed run", run)
	}
	if runner.calls[probes.LockChains] != 2 {
		t.Fatalf("lock_chains runs = %d, want 2 (one retry)", runner.calls[probes.LockChains])
	}
}

// callerRecorder records the caller class of every probe it passes on.
type callerRecorder struct {
	base    ProbeRunner
	mu      sync.Mutex
	callers []probes.Caller
}

func (r *callerRecorder) Run(ctx context.Context, id probes.ID, a probes.Args) probes.Result {
	r.mu.Lock()
	r.callers = append(r.callers, probes.CallerOf(ctx))
	r.mu.Unlock()
	return r.base.Run(ctx, id, a)
}

func TestCoordinator_PlanProbesAreTaggedInvestigation(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	r := &callerRecorder{base: idleChainRunner()}
	c, _ := testCoordinator(t, ctx, st, r, nil)
	if inv := startAndRun(t, ctx, c, lockTrigger("caller-tag")); inv.State != StateConcluded {
		t.Fatalf("investigation state = %s, want concluded", inv.State)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.callers) != 4 {
		t.Fatalf("probe runs = %d, want the lock plan's 4", len(r.callers))
	}
	for _, got := range r.callers {
		if got != probes.CallerInvestigation {
			t.Fatalf("plan probe caller = %q, want investigation", got)
		}
	}
}
