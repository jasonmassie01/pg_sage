package sre

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The investigator loop on the real store with scripted probe results:
// trigger -> claim -> probe plan per step -> causal diagnosis from the
// stored evidence -> persisted hypotheses (CHECK-33: no LLM, no external
// telemetry).

func TestCoordinator_LockIncidentConcludesFromStoredEvidence(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	runner := idleChainRunner()
	c, _ := testCoordinator(t, ctx, st, runner, nil)
	inv := startAndRun(t, ctx, c, lockTrigger("inc-1"))
	if inv.State != StateConcluded || inv.Summary.Root != "idle_in_tx_holder" ||
		inv.Summary.Subject != "pid 4242" || inv.IncidentID != "inc-1" ||
		inv.ProbeCount != 4 || inv.LeaseOwner != "" {
		t.Fatalf("investigation = %+v", inv)
	}
	hs, _ := st.Hypotheses(ctx, inv.Scope, inv.ID)
	ev, _ := st.Evidence(ctx, inv.Scope, inv.ID)
	inScope := map[UUID]bool{}
	for _, e := range ev {
		inScope[e.ID] = true
	}
	nodes := map[string]HypothesisStatus{}
	for _, h := range hs {
		nodes[h.Node] = h.Status
		for _, f := range append(h.Support, h.Contradict...) {
			if !inScope[f.EvidenceID] {
				t.Fatalf("%s cites %s outside the investigation", h.Node, f.EvidenceID)
			}
		}
		if h.RefutationProbe == "" {
			t.Fatalf("%s has no refutation probe (CHECK-37)", h.Node)
		}
	}
	if nodes["idle_in_tx_holder"] != HypothesisRoot ||
		nodes["ddl_lock_queue"] != HypothesisContributing ||
		nodes["sage_own_action"] != HypothesisRuledOut {
		t.Fatalf("hypothesis statuses = %v", nodes)
	}
	if err := st.VerifyEvents(ctx, inv.Scope, inv.ID); err != nil {
		t.Fatalf("event chain: %v", err)
	}
}

func TestCoordinator_ConnectionPlanSamplesTwice(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	c, slept := testCoordinator(t, ctx, st, leakRunner(), nil)
	inv := startAndRun(t, ctx, c, Trigger{CaseID: "incident:db:connections_high:9",
		Kind: TriggerConnections, Subject: "incident 9", IdempotencyKey: "incident:9"})
	if inv.State != StateConcluded || inv.Summary.Root != "connection_leak" {
		t.Fatalf("investigation = %+v", inv)
	}
	if len(*slept) != 1 || (*slept)[0] != 5*time.Second {
		t.Fatalf("sample delays = %v, want one of the 5 s sample interval", *slept)
	}
	ev, _ := st.Evidence(ctx, inv.Scope, inv.ID)
	samples := 0
	for _, e := range ev {
		if e.ProbeID == string(probes.ConnectionSaturation) {
			samples++
		}
	}
	if samples != 2 {
		t.Fatalf("connection samples = %d, want 2", samples)
	}
}

// CHECK-08: probes that cannot run make the investigation inconclusive
// and are named as missing evidence, never read as healthy.
func TestCoordinator_UnavailableProbesAreInconclusiveAndNamed(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	denied := func(id probes.ID) probes.Result {
		return probes.Result{ProbeID: id, Version: "v1", Status: probes.StatusNoPrivilege,
			Reason: "insufficient_privilege", Error: "permission denied"}
	}
	runner := newScriptedRunner()
	for _, id := range []probes.ID{probes.LockGraph, probes.PreparedXacts,
		probes.LongTransactions, probes.SageActions} {
		runner.script(id, denied(id))
	}
	c, _ := testCoordinator(t, ctx, st, runner, nil)
	inv := startAndRun(t, ctx, c, lockTrigger("inc-2"))
	if inv.State != StateInconclusive || inv.Summary.Conclusive || inv.Summary.Reason == "" {
		t.Fatalf("investigation = %+v, want inconclusive with a reason", inv)
	}
	named := map[string]string{}
	for _, m := range inv.Summary.Missing {
		named[m.ProbeID] = m.Status
	}
	for _, id := range []string{"lock_graph", "prepared_xacts", "sage_actions"} {
		if named[id] != "no_privilege" {
			t.Errorf("missing evidence %v lacks %s as no_privilege", named, id)
		}
	}
}

// Concurrency: two coordinators (two workers) racing the same incident
// create one investigation, run its probes once and conclude it once.
func TestCoordinator_TwoCoordinatorsRaceTheSameIncident(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	runner := idleChainRunner()
	a, _ := testCoordinator(t, ctx, st, runner, nil)
	b, err := NewCoordinator(CoordinatorDeps{Store: st, Runner: runner, Config: a.cfg,
		LogFn: func(string, string, ...any) {}})
	if err != nil {
		t.Fatalf("second coordinator: %v", err)
	}
	b.sleep = a.sleep
	if _, err := b.Bind(ctx); err != nil {
		t.Fatalf("bind b: %v", err)
	}
	if sa, _ := a.Scope(); sa != mustScope(t, b) {
		t.Fatal("two coordinators of one runtime key bound different scopes")
	}
	var wg sync.WaitGroup
	ids := make([]UUID, 2)
	errs := make([]error, 2)
	for i, c := range []*Coordinator{a, b} {
		wg.Add(1)
		go func(i int, c *Coordinator) {
			defer wg.Done()
			inv, _, err := c.Start(ctx, lockTrigger("inc-race"))
			if err != nil {
				errs[i] = err
				return
			}
			ids[i] = inv.ID
			errs[i] = c.Investigate(ctx, inv.ID)
		}(i, c)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("racing coordinator: %v", err)
		}
	}
	if ids[0] != ids[1] {
		t.Fatalf("two investigations for one incident: %s and %s", ids[0], ids[1])
	}
	var revisions, steps int
	_ = pool.QueryRow(ctx, `SELECT count(DISTINCT revision) FROM sage.sre_hypotheses
		WHERE investigation_id = $1`, string(ids[0])).Scan(&revisions)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM sage.sre_steps
		WHERE investigation_id = $1`, string(ids[0])).Scan(&steps)
	if revisions != 1 || steps != 1 || runner.total() != 4 {
		t.Fatalf("revisions %d, steps %d, probe calls %d: want 1, 1, 4",
			revisions, steps, runner.total())
	}
}

// State transitions: a crash after the first of two steps resumes at the
// second step on another worker, keeping the first step's evidence.
func TestCoordinator_ResumesAnOrphanedInvestigationAtTheNextStep(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	runner := leakRunner()
	c, _ := testCoordinator(t, ctx, st, runner, nil)
	scope, _ := c.Scope()
	inv, _, err := c.Start(ctx, Trigger{CaseID: "case:conn", Kind: TriggerConnections,
		Subject: "incident 10", IdempotencyKey: "incident:10"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	dead, err := st.Claim(ctx, scope, inv.ID, NewUUID())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	first := runner.Run(ctx, probes.ConnectionSaturation, probes.Args{})
	if _, err := st.CommitStep(ctx, dead, step("step-1", StateCollecting, first,
		runner.Run(ctx, probes.LockGraph, probes.Args{}))); err != nil {
		t.Fatalf("dead worker step: %v", err)
	}
	expireLease(t, ctx, pool, dead)
	pending, _ := st.Pending(ctx, scope, 10)
	if len(pending) != 1 || pending[0] != inv.ID {
		t.Fatalf("pending = %v, want the orphaned investigation", pending)
	}
	if err := c.Investigate(ctx, inv.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	got, _ := st.Get(ctx, scope, inv.ID)
	if got.State != StateConcluded || got.Summary.Root != "connection_leak" {
		t.Fatalf("resumed investigation = %+v", got)
	}
	if n := runner.calls[probes.ConnectionSaturation]; n != 2 {
		t.Fatalf("connection_saturation ran %d times, want 2 (no step repeated)", n)
	}
}

func TestCoordinator_PausedOrConcludedWorkIsNotRun(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	runner := idleChainRunner()
	c, _ := testCoordinator(t, ctx, st, runner, nil)
	scope, _ := c.Scope()
	inv, _, _ := c.Start(ctx, lockTrigger("inc-3"))
	if _, err := st.Pause(ctx, scope, inv.ID, inv.Version); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := c.Investigate(ctx, inv.ID); err != nil || runner.total() != 0 {
		t.Fatalf("paused investigate = %v with %d probe calls", err, runner.total())
	}
	done := startAndRun(t, ctx, c, lockTrigger("inc-4"))
	calls := runner.total()
	if err := c.Investigate(ctx, done.ID); err != nil || runner.total() != calls {
		t.Fatalf("re-investigating a concluded investigation = %v", err)
	}
}

// Lease lost mid-run (an operator stops the investigation during a
// probe): the worker commits nothing more and does not conclude.
func TestCoordinator_StoppedMidRunCommitsNothing(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	runner := idleChainRunner()
	c, _ := testCoordinator(t, ctx, st, runner, nil)
	scope, _ := c.Scope()
	inv, _, _ := c.Start(ctx, lockTrigger("inc-5"))
	runner.hook = func(id probes.ID, _ int) {
		if id == probes.LockGraph {
			cur, _ := st.Get(ctx, scope, inv.ID)
			if _, err := st.Stop(ctx, scope, inv.ID, cur.Version); err != nil {
				t.Errorf("stop: %v", err)
			}
		}
	}
	if err := c.Investigate(ctx, inv.ID); err != nil {
		t.Fatalf("investigate after a stop = %v, want a quiet exit", err)
	}
	got, _ := st.Get(ctx, scope, inv.ID)
	hs, _ := st.Hypotheses(ctx, scope, inv.ID)
	if got.State != StateCancelled || got.ProbeCount != 0 || len(hs) != 0 {
		t.Fatalf("after stop: %+v with %d hypotheses", got, len(hs))
	}
}

func TestCoordinator_PlanRegressionDiagnosesTheTriggeredQuery(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	flip := func(q int64, before, after float64) probes.Row {
		return probes.Row{"queryid": q, "previous_plan_hash": "v1:a",
			"current_plan_hash": "v1:b", "plan_flipped": true,
			"flipped_at":   time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC),
			"before_calls": int64(20), "before_mean_ms": before,
			"after_calls": int64(20), "after_mean_ms": after}
	}
	runner := newScriptedRunner().script(probes.PlanRegressions,
		rows(probes.PlanRegressions, flip(1, 1, 50), flip(2, 1, 4)))
	c, _ := testCoordinator(t, ctx, st, runner, nil)
	plan := func(q string) Trigger {
		return Trigger{CaseID: "finding:db:plan_regression:query:queryid:" + q,
			Kind: TriggerPlan, Subject: "queryid " + q, IdempotencyKey: "finding:" + q}
	}
	inv := startAndRun(t, ctx, c, plan("2"))
	if inv.State != StateConcluded || inv.Summary.Subject != "queryid 2" ||
		inv.Summary.Root != "plan_flip_regression" {
		t.Fatalf("plan investigation = %+v", inv)
	}
	other := startAndRun(t, ctx, c, plan("3"))
	if other.State != StateInconclusive ||
		!strings.Contains(other.Summary.Reason, "queryid 3") {
		t.Fatalf("unregressed query = %+v, want inconclusive naming it", other)
	}
}

// A trigger without a probe plan fails explicitly instead of hanging.
func TestCoordinator_TriggerWithoutAPlanFails(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	c, _ := testCoordinator(t, ctx, st, newScriptedRunner(), nil)
	inv := startAndRun(t, ctx, c, Trigger{CaseID: "case:op", Kind: TriggerOperator,
		Subject: "operator"})
	if inv.State != StateFailed || inv.FailureCode != "no_probe_plan" {
		t.Fatalf("operator trigger = %+v, want failed/no_probe_plan", inv)
	}
}

func TestCoordinator_RejectsInvalidDependencies(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	good := CoordinatorDeps{Store: st, Runner: newScriptedRunner(),
		Config: DefaultCoordinatorConfig("k")}
	bad := map[string]func(*CoordinatorDeps){
		"nil store":        func(d *CoordinatorDeps) { d.Store = nil },
		"nil runner":       func(d *CoordinatorDeps) { d.Runner = nil },
		"empty key":        func(d *CoordinatorDeps) { d.Config.RuntimeKey = "" },
		"zero sample":      func(d *CoordinatorDeps) { d.Config.SampleInterval = 0 },
		"sample over 30s":  func(d *CoordinatorDeps) { d.Config.SampleInterval = 31 * time.Second },
		"zero trigger":     func(d *CoordinatorDeps) { d.Config.TriggerInterval = 0 },
		"queue 0":          func(d *CoordinatorDeps) { d.Config.QueueSize = 0 },
		"queue over 100":   func(d *CoordinatorDeps) { d.Config.QueueSize = 101 },
		"bad retention":    func(d *CoordinatorDeps) { d.Config.Retention.BatchSize = 0 },
		"zero action span": func(d *CoordinatorDeps) { d.Config.ActionWindow = 0 },
		"auto without source": func(d *CoordinatorDeps) {
			d.Config.AutomaticStart, d.Triggers = true, nil
		},
	}
	for name, mutate := range bad {
		d := good
		mutate(&d)
		if _, err := NewCoordinator(d); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: NewCoordinator = %v, want ErrInvalidRequest", name, err)
		}
	}
	c, err := NewCoordinator(good)
	if err != nil || c == nil {
		t.Fatalf("valid deps: %v", err)
	}
	if _, _, err := c.Start(ctx, lockTrigger("x")); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("start before bind = %v, want ErrInvalidRequest", err)
	}
}

func mustScope(t *testing.T, c *Coordinator) Scope {
	t.Helper()
	s, ok := c.Scope()
	if !ok {
		t.Fatal("coordinator not bound")
	}
	return s
}
