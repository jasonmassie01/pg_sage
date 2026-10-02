package sre

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/sre/runbook"
)

// A signed runbook matching an investigation's trigger extends its probe
// plan within the probe ceiling and active time, decides deterministically
// on its evidence and the causal graph, and records which version ran and
// what it proposes (never executed). Unsigned, edited, retired, tampered
// or non-matching runbooks never run (AI-SRE-SPEC §7.1).

func runbookEvidence(t *testing.T, st *PostgresStore, inv Investigation) []Evidence {
	t.Helper()
	ev, err := st.Evidence(t.Context(), inv.Scope, inv.ID)
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	var out []Evidence
	for _, e := range ev {
		if strings.HasPrefix(e.StepKey, "runbook-") {
			out = append(out, e)
		}
	}
	return out
}

func runIdle(t *testing.T, ctx context.Context, st *PostgresStore, runner ProbeRunner,
	setup func(Scope)) (Investigation, *Coordinator) {
	t.Helper()
	c, _ := testCoordinator(t, ctx, st, runner, nil)
	scope, _ := c.Scope()
	if setup != nil {
		setup(scope)
	}
	return startAndRun(t, ctx, c, lockTrigger("rb-"+string(NewUUID())[:8])), c
}

func TestRunbookRun_SignedRunbookExtendsThePlan(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	var rb Runbook
	inv, _ := runIdle(t, ctx, st, idleChainRunner(), func(scope Scope) {
		rb = signedRunbook(t, ctx, st, scope, idleRunbook())
	})
	if inv.State != StateConcluded || inv.ProbeCount != 5 {
		t.Fatalf("investigation %s with %d probes, want concluded with the plan's 4 "+
			"plus the runbook's 1", inv.State, inv.ProbeCount)
	}
	ev := runbookEvidence(t, st, inv)
	if len(ev) != 1 || ev[0].ProbeID != string(probes.LockChains) {
		t.Fatalf("runbook evidence = %+v, want one lock_chains observation", ev)
	}
	run := inv.Summary.Runbook
	node, _ := causal.NodeByID(causal.IdleInTxHolder)
	if run == nil || run.RunbookID != rb.ID || run.Version != 1 || run.Label != RunbookRunLabel ||
		run.ContentHash != hashOf(t, idleRunbook()) || run.SignedBy != "user:1" ||
		run.Outcome != RunbookCompleted || run.Probes != 1 ||
		strings.Join(run.Path, ",") != "read_chains,is_idle,end_tx" ||
		run.Proposal == nil || run.Proposal.Kind != "operator_step" ||
		run.Proposal.Text != node.OperatorStep || run.Proposal.Label != RunbookProposalLabel {
		t.Fatalf("summary runbook = %+v, want the completed v1 run", run)
	}
	runs, err := st.RunbookRuns(ctx, inv.Scope, rb.ID, 10)
	if err != nil || len(runs) != 1 || runs[0].InvestigationID != inv.ID ||
		runs[0].Version != 1 {
		t.Fatalf("run history = %+v, %v", runs, err)
	}
}

// unrunnable are runbook setups that must never run.
var unrunnable = map[string]func(t *testing.T, ctx context.Context, st *PostgresStore,
	s Scope){
		"unsigned draft": func(t *testing.T, ctx context.Context, st *PostgresStore, s Scope) {
			if _, err := st.CreateRunbook(ctx, s, draftOf(idleRunbook())); err != nil {
				t.Fatal(err)
			}
		},
		"edited after signing": func(t *testing.T, ctx context.Context, st *PostgresStore,
			s Scope) {
			rb := signedRunbook(t, ctx, st, s, idleRunbook())
			d := idleRunbook()
			d.Description = "edited"
			if _, err := st.ReviseRunbook(ctx, s, rb.ID, 1, draftOf(d)); err != nil {
				t.Fatal(err)
			}
		},
		"retired": func(t *testing.T, ctx context.Context, st *PostgresStore, s Scope) {
			rb := signedRunbook(t, ctx, st, s, idleRunbook())
			if _, err := st.RetireRunbook(ctx, s, rb.ID, "user:3"); err != nil {
				t.Fatal(err)
			}
		},
		"other trigger kind": func(t *testing.T, ctx context.Context, st *PostgresStore,
			s Scope) {
			d := idleRunbook()
			d.Trigger = runbook.Trigger{Kinds: []string{string(TriggerWAL)}}
			signedRunbook(t, ctx, st, s, d)
		},
		"node not open": func(t *testing.T, ctx context.Context, st *PostgresStore, s Scope) {
			d := idleRunbook()
			d.Trigger.Nodes = []string{string(causal.InactiveSlot)}
			signedRunbook(t, ctx, st, s, d)
		},
}

func TestRunbookRun_NeverRunsWithoutAValidSignature(t *testing.T) {
	for name, setup := range unrunnable {
		t.Run(name, func(t *testing.T) {
			st, _, ctx := liveStore(t, DefaultLimits())
			inv, _ := runIdle(t, ctx, st, idleChainRunner(), func(s Scope) {
				setup(t, ctx, st, s)
			})
			if inv.Summary.Runbook != nil || inv.ProbeCount != 4 ||
				len(runbookEvidence(t, st, inv)) != 0 {
				t.Fatalf("%s: runbook ran (%+v, %d probes)", name, inv.Summary.Runbook,
					inv.ProbeCount)
			}
			if inv.State != StateConcluded {
				t.Fatalf("%s: investigation %s, want the deterministic conclusion", name,
					inv.State)
			}
		})
	}
}

func TestRunbookRun_ElseBranchProposesATypedActionWithoutExecuting(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	d := idleRunbook()
	d.Nodes[1].When = &runbook.Predicate{Op: runbook.OpHypothesis,
		Node: string(causal.PreparedXactHolder), In: []string{"root_cause"}}
	d.Nodes[3].Proposal = &runbook.Proposal{Kind: runbook.ProposalAction,
		ActionType: "cancel_backend", Node: string(causal.IdleInTxHolder)}
	inv, _ := runIdle(t, ctx, st, idleChainRunner(), func(s Scope) {
		signedRunbook(t, ctx, st, s, d)
	})
	run := inv.Summary.Runbook
	if run == nil || strings.Join(run.Path, ",") != "read_chains,is_idle,escalate" ||
		run.Proposal == nil || run.Proposal.Kind != "action" ||
		run.Proposal.ActionType != "cancel_backend" ||
		!strings.Contains(run.Proposal.Text, "not executed") {
		t.Fatalf("runbook run = %+v, want the else branch's action proposal", run)
	}
}

func TestRunbookRun_DecidesOnItsOwnEvidence(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	runner := idleChainRunner().script(probes.LockChains, rows(probes.LockChains,
		probes.Row{"root_pid": int64(4242), "root_kind": "backend",
			"total_blocked": int64(2), "chain_depth": int64(2)}))
	d := idleRunbook()
	d.Nodes[1].When = &runbook.Predicate{Op: runbook.OpColumn, Probe: "lock_chains",
		Column: "total_blocked", Agg: runbook.AggMax, Cmp: ">=", Value: ptr(2.0)}
	inv, _ := runIdle(t, ctx, st, runner, func(s Scope) { signedRunbook(t, ctx, st, s, d) })
	if run := inv.Summary.Runbook; run == nil || run.Path[len(run.Path)-1] != "end_tx" {
		t.Fatalf("runbook run = %+v, want the then branch from lock_chains' 2 blocked", run)
	}
}

func ptr(v float64) *float64 { return &v }

func TestRunbookRun_AbstainsWhenADecisionCannotBeMade(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	runner := idleChainRunner().script(probes.LockChains, probes.Result{
		ProbeID: probes.LockChains, Version: "v1", Status: probes.StatusError,
		Reason: "statement_timeout"})
	d := idleRunbook()
	d.Nodes[1].When = &runbook.Predicate{Op: runbook.OpColumn, Probe: "lock_chains",
		Column: "total_blocked", Agg: runbook.AggMax, Cmp: ">=", Value: ptr(2.0)}
	inv, _ := runIdle(t, ctx, st, runner, func(s Scope) { signedRunbook(t, ctx, st, s, d) })
	run := inv.Summary.Runbook
	if run == nil || run.Outcome != RunbookAbstained || run.Proposal != nil ||
		!strings.Contains(run.Reason, "lock_chains") ||
		strings.Join(run.Path, ",") != "read_chains,is_idle" {
		t.Fatalf("runbook run = %+v, want an abstention naming lock_chains", run)
	}
	if inv.State != StateConcluded {
		t.Fatalf("an abstaining runbook changed the investigation to %s", inv.State)
	}
}

func TestRunbookRun_StaysWithinTheProbeCeiling(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxProbes = 5
	st, _, ctx := liveStore(t, limits)
	d := idleRunbook()
	d.Nodes[0].Next = "read_long"
	d.Nodes = append(d.Nodes,
		runbook.Node{ID: "read_long", Type: runbook.NodeProbe, Probe: "long_transactions",
			Next: "read_prepared"},
		runbook.Node{ID: "read_prepared", Type: runbook.NodeProbe, Probe: "prepared_xacts",
			Next: "is_idle"})
	inv, _ := runIdle(t, ctx, st, idleChainRunner(), func(s Scope) {
		signedRunbook(t, ctx, st, s, d)
	})
	run := inv.Summary.Runbook
	if inv.ProbeCount != 5 || run == nil || run.Outcome != RunbookProbeBudget ||
		run.Probes != 1 || run.Proposal != nil ||
		strings.Join(run.Path, ",") != "read_chains,read_long" {
		t.Fatalf("probes %d, run %+v; want the ceiling of 5 and a budget stop at read_long",
			inv.ProbeCount, run)
	}
}

func TestRunbookRun_MostSpecificRunbookRunsAlone(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	general := idleRunbook()
	general.Name = "General lock runbook"
	general.Trigger.Nodes = nil
	var specific Runbook
	inv, _ := runIdle(t, ctx, st, idleChainRunner(), func(s Scope) {
		signedRunbook(t, ctx, st, s, general)
		specific = signedRunbook(t, ctx, st, s, idleRunbook())
	})
	if run := inv.Summary.Runbook; run == nil || run.RunbookID != specific.ID {
		t.Fatalf("ran %+v, want the node-specific runbook %s", run, specific.ID)
	}
	if n := len(runbookEvidence(t, st, inv)); n != 1 {
		t.Fatalf("%d runbook probes ran, want one runbook's 1", n)
	}
}

// failingRunbooks is a store whose runbook lookup fails.
type failingRunbooks struct {
	*PostgresStore
	err error
}

func (f failingRunbooks) RunnableRunbooks(context.Context, Scope) ([]Runbook, error) {
	return nil, f.err
}

func TestRunbookRun_LookupFailureKeepsTheDeterministicInvestigation(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	logs := &logLines{}
	cfg := DefaultCoordinatorConfig("test:" + string(NewUUID()))
	c, err := NewCoordinator(CoordinatorDeps{Store: failingRunbooks{st,
		ErrMetadataUnavailable}, Runner: idleChainRunner(), Config: cfg, LogFn: logs.logFn})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Bind(ctx); err != nil {
		t.Fatal(err)
	}
	inv := startAndRun(t, ctx, c, lockTrigger("rb-fail"))
	if inv.State != StateConcluded || inv.Summary.Runbook != nil {
		t.Fatalf("investigation %s runbook %+v; a failed lookup must not block it",
			inv.State, inv.Summary.Runbook)
	}
	if logs.count("runbook") == 0 {
		t.Fatal("the failed runbook lookup was not logged")
	}
}

func TestRunbookRun_ResumesFromNeedsEvidence(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	c, _ := testCoordinator(t, ctx, st, idleChainRunner(), nil)
	scope, _ := c.Scope()
	signedRunbook(t, ctx, st, scope, idleRunbook())
	inv, _, err := c.Start(ctx, lockTrigger("rb-resume"))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
	if err != nil {
		t.Fatal(err)
	}
	plan, _ := planFor(TriggerLock, c.cfg.ActionWindow)
	if _, err := st.CommitStep(ctx, lease, StepResult{IdempotencyKey: stepKey(0),
		Results: c.runStep(ctx, plan[0]), NextState: StateEvaluating}); err != nil {
		t.Fatalf("commit the plan step: %v", err)
	}
	// The worker stops while waiting for more evidence.
	if _, err := st.Release(ctx, lease, StateNeedsEvidence); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := c.Investigate(ctx, inv.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	got, _ := st.Get(ctx, inv.Scope, inv.ID)
	if got.State != StateConcluded || got.Summary.Runbook == nil ||
		got.Summary.Runbook.Outcome != RunbookCompleted {
		t.Fatalf("resumed from needs_evidence to %s (runbook %+v), want concluded "+
			"with the runbook run", got.State, got.Summary.Runbook)
	}
}

func TestRunbookRun_StaleLeaseRecordsNothing(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	lease := claimed(t, st, "pid 78")
	signedRunbook(t, ctx, st, lease.Scope, idleRunbook())
	stale := lease
	stale.Fence++
	err := st.RecordRunbookRun(ctx, stale, RunbookRun{Label: RunbookRunLabel,
		RunbookID: NewUUID(), Version: 1, ContentHash: strings.Repeat("a", 64),
		Outcome: RunbookCompleted, Path: []string{"x"}})
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("record under a stale fence = %v, want ErrLeaseLost", err)
	}
}
