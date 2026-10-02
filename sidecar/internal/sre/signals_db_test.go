package sre

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Sage SRE M5 signals on the real store: when a change feed and an SLO
// engine are wired, every investigation also collects change-feed and
// SLO-status evidence ("what changed?", "are customers hurt?"), and a
// page-level SLO burn starts its own investigation that triages the
// lock, connection and plan mechanisms.

type signalStub struct {
	mu    sync.Mutex
	calls map[probes.ID]int
	args  map[probes.ID]probes.Args
	rows  map[probes.ID][]probes.Row
}

func newSignalStub() *signalStub {
	return &signalStub{calls: map[probes.ID]int{}, args: map[probes.ID]probes.Args{},
		rows: map[probes.ID][]probes.Row{}}
}

func (s *signalStub) probe(id probes.ID) SignalProbe {
	return SignalProbe{ID: id, Run: func(_ context.Context, a probes.Args) probes.Result {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.calls[id]++
		s.args[id] = a
		res := probes.Result{ProbeID: id, Version: "v1", Status: probes.StatusEmpty,
			ObservedAt: time.Now()}
		if rs := s.rows[id]; len(rs) > 0 {
			res.Status, res.Rows = probes.StatusOK, rs
		}
		return res
	}}
}

func burningSLO() probes.Row {
	return probes.Row{"name": "checkout", "kind": "app", "state": "page",
		"fast_burning": true, "customer_impact": true, "burn_long": 16.5,
		"burn_short": 15.0, "long_window": "1h", "short_window": "5m", "unknown": "",
		"budget_remaining": 0.3, "evaluated_at": time.Now().UTC(), "age_s": 10.0}
}

func deployRow() probes.Row {
	return probes.Row{"kind": "deploy", "source": "github-actions",
		"summary": "deploy checkout v1.2.3", "occurred_at": time.Now().UTC().Add(-2 * time.Minute),
		"age_s": 120.0, "signature": "verified", "event_id": "run-1", "service": "checkout"}
}

func signalCoordinator(t *testing.T, ctx context.Context, st *PostgresStore,
	runner ProbeRunner, stub *signalStub) *Coordinator {
	t.Helper()
	cfg := DefaultCoordinatorConfig("signals:" + string(NewUUID()))
	c, err := NewCoordinator(CoordinatorDeps{Store: st, Runner: runner, Config: cfg,
		Signals: []SignalProbe{stub.probe(probes.ChangeFeed), stub.probe(probes.SLOStatus)}})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	c.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	if _, err := c.Bind(ctx); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewCoordinator_RejectsInvalidSignals(t *testing.T) {
	st, _, _ := liveStore(t, DefaultLimits())
	cfg := DefaultCoordinatorConfig("signals:" + string(NewUUID()))
	run := func(context.Context, probes.Args) probes.Result { return probes.Result{} }
	for name, sigs := range map[string][]SignalProbe{
		"sql probe": {{ID: probes.LockGraph, Run: run}},
		"no func":   {{ID: probes.ChangeFeed}},
		"duplicate": {{ID: probes.ChangeFeed, Run: run}, {ID: probes.ChangeFeed, Run: run}},
	} {
		_, err := NewCoordinator(CoordinatorDeps{Store: st, Runner: newScriptedRunner(),
			Config: cfg, Signals: sigs})
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
}

// A lock investigation with signals: the change feed answers "what
// changed?" with a cited hypothesis, the SLO status binds customer
// impact, and the lock root is unchanged.
func TestCoordinator_SignalsJoinEveryInvestigation(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	stub := newSignalStub()
	stub.rows[probes.ChangeFeed] = []probes.Row{deployRow()}
	stub.rows[probes.SLOStatus] = []probes.Row{burningSLO()}
	c := signalCoordinator(t, ctx, st, idleChainRunner(), stub)
	inv := startAndRun(t, ctx, c, lockTrigger("sig-1"))
	if inv.State != StateConcluded || inv.Summary.Root != "idle_in_tx_holder" {
		t.Fatalf("investigation = %+v", inv)
	}
	if stub.calls[probes.ChangeFeed] != 1 || stub.calls[probes.SLOStatus] != 1 ||
		stub.args[probes.ChangeFeed].Window != time.Hour {
		t.Fatalf("signal calls = %v args = %v", stub.calls, stub.args)
	}
	imp := inv.Summary.CustomerImpact
	if imp == nil || imp.State != "burning" || imp.SLO != "checkout" ||
		imp.BurnRate == nil || *imp.BurnRate != 16.5 {
		t.Fatalf("customer impact = %+v", imp)
	}
	ev, _ := st.Evidence(ctx, inv.Scope, inv.ID)
	ids := map[string]UUID{}
	for _, e := range ev {
		ids[e.ProbeID] = e.ID
	}
	if ids["change_feed"] == "" || ids["slo_status"] == "" || imp.EvidenceID != ids["slo_status"] {
		t.Fatalf("evidence = %v, impact cites %s", ids, imp.EvidenceID)
	}
	hs, _ := st.Hypotheses(ctx, inv.Scope, inv.ID)
	var change *HypothesisRecord
	for i := range hs {
		if hs[i].Node == "recent_change" {
			change = &hs[i]
		}
	}
	if change == nil || change.Status != HypothesisUnproven || len(change.Support) != 1 ||
		change.Support[0].EvidenceID != ids["change_feed"] ||
		!strings.Contains(change.Support[0].Text, "deploy checkout v1.2.3") {
		t.Fatalf("recent_change hypothesis = %+v", change)
	}
}

// A page-level burn starts an slo_burn investigation that triages the
// database mechanisms and concludes on the lock chain it finds.
func TestCoordinator_SLOBurnInvestigationFindsTheLockChain(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	stub := newSignalStub()
	stub.rows[probes.SLOStatus] = []probes.Row{burningSLO()}
	c := signalCoordinator(t, ctx, st, idleChainRunner(), stub)
	tr := Trigger{CaseID: "slo:checkout", Kind: TriggerSLOBurn, Subject: "slo checkout",
		IdempotencyKey: "slo:checkout:1759320000"}
	inv := startAndRun(t, ctx, c, tr)
	if inv.State != StateConcluded || inv.Summary.Family != "slo_burn" ||
		inv.Summary.Root != "idle_in_tx_holder" || inv.Summary.Subject != "slo checkout" {
		t.Fatalf("investigation = %+v", inv)
	}
	if inv.ProbeCount > st.Limits().MaxProbes || inv.ProbeCount < 6 {
		t.Fatalf("probe count %d", inv.ProbeCount)
	}
	again, created, err := c.Start(ctx, tr)
	if err != nil || created || again.ID != inv.ID {
		t.Fatalf("same burn restarted: created=%v err=%v", created, err)
	}
}

// No database mechanism: inconclusive, and the reason says the cause may
// be outside PostgreSQL.
func TestCoordinator_SLOBurnWithoutMechanismIsInconclusive(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	stub := newSignalStub()
	stub.rows[probes.SLOStatus] = []probes.Row{burningSLO()}
	c := signalCoordinator(t, ctx, st, newScriptedRunner(), stub)
	inv := startAndRun(t, ctx, c, Trigger{CaseID: "slo:checkout", Kind: TriggerSLOBurn,
		Subject: "slo checkout", IdempotencyKey: "slo:checkout:2"})
	if inv.State != StateInconclusive || !strings.Contains(inv.Summary.Reason,
		"outside PostgreSQL") || inv.Summary.CustomerImpact == nil {
		t.Fatalf("investigation = %+v", inv)
	}
}

// The slo_burn kind is a valid trigger; without signals every plan is the
// M2 plan (no extra probes).
func TestTriggerSLOBurn_Valid(t *testing.T) {
	r := StartRequest{Scope: Scope{DeploymentID: NewUUID(), DatabaseID: NewUUID()},
		CaseID: "slo:checkout", TriggerKind: TriggerSLOBurn}
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	plain, ok := planFor(TriggerLock, time.Hour)
	if !ok || len(plain) != 1 || len(plain[0].calls) != 4 {
		t.Fatalf("lock plan without signals = %+v", plain)
	}
	burn, ok := planFor(TriggerSLOBurn, time.Hour)
	if !ok || len(burn) == 0 {
		t.Fatalf("slo_burn plan = %+v", burn)
	}
}
