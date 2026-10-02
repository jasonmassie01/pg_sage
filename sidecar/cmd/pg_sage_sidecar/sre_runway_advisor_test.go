package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/autonomy"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/sre"
)

// The runway advisor attaches the existing custodian actions (freeze, WAL
// bound) that address a concluded runway diagnosis, with the standing
// gate's verdict explained (never recorded, never executed). Sequences
// have no custodian action: the advisor says so.

type scanStub struct {
	out   []autonomy.Proposal
	err   error
	scans int
}

func (s *scanStub) Scan(context.Context) ([]autonomy.Proposal, error) {
	s.scans++
	return s.out, s.err
}

type explainStub struct {
	authorized, explained int
	verdict               policy.Decision
}

func (g *explainStub) Authorize(context.Context, policy.ActionRequest) policy.Decision {
	g.authorized++
	return g.verdict
}

func (g *explainStub) Explain(context.Context, policy.ActionRequest) policy.Decision {
	g.explained++
	return g.verdict
}

func advisorWith(freeze, wal *scanStub, gate *explainStub) *runwayAdvisor {
	a := newRunwayAdvisor(freeze, wal, func(context.Context) bool { return false })
	if gate != nil {
		exec := executor.New(nil, &config.Config{}, time.Time{}, func(string, string, ...any) {})
		exec.WithPolicyGate(gate)
		a.attach(exec)
	}
	return a
}

func freezeProposal(table string) autonomy.Proposal {
	return autonomy.Proposal{Feature: "freeze", TargetObjects: []string{table},
		SQL: "VACUUM (FREEZE) " + table}
}

func TestRunwayAdvisor_WraparoundFreezeForTheSubjectTable(t *testing.T) {
	freeze := &scanStub{out: []autonomy.Proposal{freezeProposal("public.a"),
		freezeProposal("public.events")}}
	gate := &explainStub{verdict: policy.Decision{Verdict: policy.VerdictQueueApproval,
		RiskTier: policy.RiskSafe, Reason: "approval required"}}
	a := advisorWith(freeze, &scanStub{}, gate)
	ps, err := a.Advise(context.Background(), sre.AdviceRequest{
		Kind: sre.TriggerWraparound, Subject: "table public.events",
		Root: "xmin_held_by_session"})
	if err != nil || len(ps) != 1 {
		t.Fatalf("proposals = %+v (%v)", ps, err)
	}
	p := ps[0]
	if p.Feature != "freeze" || p.Targets[0] != "public.events" ||
		p.Verdict != string(executor.PolicyDecisionQueueApproval) ||
		p.Reason != "approval required" {
		t.Fatalf("proposal = %+v", p)
	}
	if gate.explained != 1 || gate.authorized != 0 {
		t.Fatalf("explained %d authorized %d; the advisor must never authorize",
			gate.explained, gate.authorized)
	}
	// A cluster runway ("xid") takes the custodian's proposals, at most 3.
	freeze.out = append(freeze.out, freezeProposal("public.b"), freezeProposal("public.c"))
	ps, err = a.Advise(context.Background(), sre.AdviceRequest{
		Kind: sre.TriggerWraparound, Subject: "xid", Root: "autovacuum_saturated"})
	if err != nil || len(ps) != 3 {
		t.Fatalf("cluster proposals = %d (%v), want at most 3", len(ps), err)
	}
}

func TestRunwayAdvisor_DiskWALBoundAndEscalation(t *testing.T) {
	wal := &scanStub{out: []autonomy.Proposal{
		{Feature: "wal", SQL: "ALTER SYSTEM SET max_slot_wal_keep_size = '15360MB'",
			TargetObjects: []string{"slot:cdc"}},
		{Feature: "wal", Plan: "Escalate WAL retention on slot sub: registered consumer",
			TargetObjects: []string{"slot:sub"}}}}
	gate := &explainStub{verdict: policy.Decision{Verdict: policy.VerdictExecute,
		RiskTier: policy.RiskSafe}}
	ps, err := advisorWith(&scanStub{}, wal, gate).Advise(context.Background(),
		sre.AdviceRequest{Kind: sre.TriggerDiskWAL, Subject: "slot cdc",
			Root: "inactive_slot"})
	if err != nil || len(ps) != 2 {
		t.Fatalf("proposals = %+v (%v)", ps, err)
	}
	if ps[0].SQL == "" || ps[0].Verdict != string(executor.PolicyDecisionExecute) ||
		!strings.Contains(ps[0].Action, "max_slot_wal_keep_size") {
		t.Fatalf("bound = %+v", ps[0])
	}
	if ps[1].SQL != "" || ps[1].Verdict != "manual_only" ||
		!strings.Contains(ps[1].Action, "Escalate") {
		t.Fatalf("escalation = %+v, want a plan for a human", ps[1])
	}
	// Database growth has no custodian action.
	ps, err = advisorWith(&scanStub{}, wal, gate).Advise(context.Background(),
		sre.AdviceRequest{Kind: sre.TriggerDiskWAL, Subject: "disk",
			Root: "database_growth"})
	if err != nil || len(ps) != 0 {
		t.Fatalf("database growth proposals = %+v (%v)", ps, err)
	}
}

func TestRunwayAdvisor_SequenceIsManualOnly(t *testing.T) {
	ps, err := advisorWith(&scanStub{}, &scanStub{}, &explainStub{}).Advise(
		context.Background(), sre.AdviceRequest{Kind: sre.TriggerSequence,
			Subject: "sequence public.orders_id_seq",
			Root:    "column_narrower_than_sequence"})
	if err != nil || len(ps) != 1 || ps[0].Verdict != "manual_only" || ps[0].SQL != "" ||
		ps[0].Targets[0] != "public.orders_id_seq" {
		t.Fatalf("sequence proposals = %+v (%v)", ps, err)
	}
}

// Error propagation: no executor yet, or a failing custodian scan, is an
// error the coordinator logs; nothing is proposed.
func TestRunwayAdvisor_Errors(t *testing.T) {
	_, err := advisorWith(&scanStub{}, &scanStub{}, nil).Advise(context.Background(),
		sre.AdviceRequest{Kind: sre.TriggerWraparound, Subject: "xid"})
	if err == nil || !strings.Contains(err.Error(), "executor") {
		t.Fatalf("no executor = %v", err)
	}
	broken := &scanStub{err: errors.New("pg_class unreadable")}
	_, err = advisorWith(broken, &scanStub{}, &explainStub{}).Advise(context.Background(),
		sre.AdviceRequest{Kind: sre.TriggerWraparound, Subject: "xid"})
	if err == nil || !strings.Contains(err.Error(), "pg_class unreadable") {
		t.Fatalf("scan failure = %v", err)
	}
	ps, err := advisorWith(&scanStub{}, &scanStub{}, &explainStub{}).Advise(
		context.Background(), sre.AdviceRequest{Kind: sre.TriggerLock})
	if err != nil || len(ps) != 0 {
		t.Fatalf("R1 kind = %+v (%v), want nothing", ps, err)
	}
}

func TestRunwayOptions_FromConfig(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Forecaster.DiskCapacityBytes = 5 << 40
	cfg.SRE.Runways.Investigate = false
	o := runwayOptions(cfg, "orders")
	if o.Database != "orders" || o.Investigate || o.DiskCapacityBytes != 5<<40 ||
		o.WALRetainedLimitBytes != float64(autonomy.DefaultWALBackstopBytes) ||
		o.MinSamples != 10 || o.Lookback != cfg.SRE.Runways.Lookback() ||
		o.SequenceCritical != cfg.SRE.Runways.SequenceCritical() {
		t.Fatalf("options = %+v", o)
	}
}
