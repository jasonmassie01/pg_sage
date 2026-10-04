package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/autonomy"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/specialist"
	"github.com/pg-sage/sidecar/internal/sre"
)

// The specialist contract's remediation requests for custodian actions:
// the custodian re-scans, and the matching proposal goes through the
// executor's one pipeline (policy.Gate, trust ledger, budgets, facts)
// exactly as if pg_sage proposed it, never as an approval.

type captureGate struct {
	decision policy.Decision
	requests []policy.ActionRequest
}

func (g *captureGate) Authorize(_ context.Context, r policy.ActionRequest) policy.Decision {
	g.requests = append(g.requests, r)
	return g.decision
}

func (g *captureGate) Explain(_ context.Context, r policy.ActionRequest) policy.Decision {
	return g.decision
}

func custodianFixture(t *testing.T, verdict policy.Verdict, replica bool) (
	*specialistCustodians, *captureGate, *scanStub) {
	t.Helper()
	freeze := &scanStub{out: []autonomy.Proposal{freezeProposal("public.events")}}
	gate := &captureGate{decision: policy.Decision{Verdict: verdict,
		RiskTier: policy.RiskSafe, Reason: policy.ReasonApprovalRequired, Detail: "L1"}}
	a := newRunwayAdvisor(freeze, &scanStub{}, func(context.Context) bool { return replica })
	exec := executor.New(nil, &config.Config{}, time.Time{}, func(string, string, ...any) {})
	exec.WithPolicyGate(gate)
	a.attach(exec)
	c := newSpecialistCustodians()
	c.register("orders", a)
	return c, gate, freeze
}

var wraparoundInv = sre.Investigation{TriggerKind: sre.TriggerWraparound,
	Subject: "table public.events", State: sre.StateConcluded,
	Summary: sre.Summary{Root: "xmin_held_by_session"}}

var freezeRemediation = sre.ActionProposal{Feature: "freeze",
	SQL: "VACUUM (FREEZE) public.events", Targets: []string{"public.events"}}

func TestSpecialistCustodians_SubmitThroughTheGateAsPgSageInitiative(t *testing.T) {
	c, gate, _ := custodianFixture(t, policy.VerdictQueueApproval, false)
	out, err := c.SubmitCustodian(context.Background(), "orders", wraparoundInv,
		freezeRemediation, "agent:pagerduty:t-1")
	if err != nil {
		t.Fatal(err)
	}
	if out.Decision != executor.PolicyDecisionQueueApproval || out.Stale ||
		out.Reason != string(policy.ReasonApprovalRequired) {
		t.Fatalf("outcome %+v", out)
	}
	if len(gate.requests) != 1 {
		t.Fatalf("gate asked %d times", len(gate.requests))
	}
	r := gate.requests[0]
	if r.OperatorApproved || r.OwnerDeclared || r.Rollback || r.IsReplica ||
		r.SQL != "VACUUM (FREEZE) public.events" ||
		r.Evidence["requested_by"] != "agent:pagerduty:t-1" ||
		r.Evidence["requested_via"] != "specialist" || r.EvidenceObservedAt.IsZero() {
		t.Fatalf("request %+v", r)
	}
}

func TestSpecialistCustodians_GateVerdicts(t *testing.T) {
	for verdict, want := range map[policy.Verdict]string{
		policy.VerdictBlocked: executor.PolicyDecisionBlocked,
		policy.VerdictPark:    executor.PolicyDecisionParked} {
		c, _, _ := custodianFixture(t, verdict, false)
		out, err := c.SubmitCustodian(context.Background(), "orders", wraparoundInv,
			freezeRemediation, "agent:x:t")
		if err != nil || out.Decision != want {
			t.Fatalf("%s: %+v %v", verdict, out, err)
		}
	}
}

func TestSpecialistCustodians_ReplicaFailsClosed(t *testing.T) {
	c, gate, _ := custodianFixture(t, policy.VerdictBlocked, true)
	if _, err := c.SubmitCustodian(context.Background(), "orders", wraparoundInv,
		freezeRemediation, "agent:x:t"); err != nil {
		t.Fatal(err)
	}
	if len(gate.requests) != 1 || !gate.requests[0].IsReplica {
		t.Fatalf("a replica must reach the gate marked as one: %+v", gate.requests)
	}
}

func TestSpecialistCustodians_StaleProposalNeverReachesTheGate(t *testing.T) {
	c, gate, freeze := custodianFixture(t, policy.VerdictExecute, false)
	freeze.out = []autonomy.Proposal{freezeProposal("public.other")}
	out, err := c.SubmitCustodian(context.Background(), "orders", wraparoundInv,
		freezeRemediation, "agent:x:t")
	if err != nil || !out.Stale || len(gate.requests) != 0 {
		t.Fatalf("stale: %+v %v (gate %d)", out, err, len(gate.requests))
	}
	// Same table, different SQL: also stale (the stored text is not run).
	freeze.out = []autonomy.Proposal{freezeProposal("public.events")}
	edited := freezeRemediation
	edited.SQL = "VACUUM (FREEZE, VERBOSE) public.events"
	out, err = c.SubmitCustodian(context.Background(), "orders", wraparoundInv, edited,
		"agent:x:t")
	if err != nil || !out.Stale || len(gate.requests) != 0 {
		t.Fatalf("edited SQL: %+v %v", out, err)
	}
}

func TestSpecialistCustodians_UnknownDatabase(t *testing.T) {
	c := newSpecialistCustodians()
	_, err := c.SubmitCustodian(context.Background(), "orders", wraparoundInv,
		freezeRemediation, "agent:x:t")
	if !errors.Is(err, specialist.ErrUnavailable) {
		t.Fatalf("no advisor: %v", err)
	}
	c2, _, _ := custodianFixture(t, policy.VerdictExecute, false)
	c2.remove("orders")
	if _, err := c2.SubmitCustodian(context.Background(), "orders", wraparoundInv,
		freezeRemediation, "agent:x:t"); !errors.Is(err, specialist.ErrUnavailable) {
		t.Fatalf("removed advisor: %v", err)
	}
}

func TestBuildSpecialist(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := fleet.NewManager(cfg)
	if rt, err := buildSpecialist(cfg, mgr, nil, nil, newSpecialistCustodians()); err != nil ||
		rt != nil {
		t.Fatalf("no control pool: %+v %v", rt, err)
	}
	cfg.Specialist.Enabled = false
	if rt, err := buildSpecialistWithStore(cfg, mgr, newSpecialistMemStore(), nil,
		newSpecialistCustodians()); err != nil || rt != nil {
		t.Fatalf("disabled: %+v %v", rt, err)
	}
	cfg.Specialist.Enabled = true
	cfg.Specialist.PagerDuty.SigningSecret = "s"
	cfg.Specialist.PagerDuty.Services = []string{"PSVC1=orders:operator"}
	if _, err := buildSpecialistWithStore(cfg, mgr, newSpecialistMemStore(), nil,
		newSpecialistCustodians()); err == nil {
		t.Fatal("an invalid PagerDuty service mapping must fail startup")
	}
	cfg.Specialist.PagerDuty.Services = []string{"PSVC1=orders:lock_blocking"}
	cfg.Specialist.PagerDuty.APIURL = "https://api.pagerduty.com"
	cfg.Specialist.PagerDuty.APIToken, cfg.Specialist.PagerDuty.FromEmail = "t", "a@b.c"
	rt, err := buildSpecialistWithStore(cfg, mgr, newSpecialistMemStore(), nil,
		newSpecialistCustodians())
	if err != nil || rt == nil || rt.handler == nil || rt.mcp == nil || rt.worker == nil ||
		rt.worker.Notifiers["pagerduty"] == nil || rt.worker.Notifiers["webhook"] != nil {
		t.Fatalf("runtime %+v %v", rt, err)
	}
}

func TestSpecialistMCPBackend_MapsTheCaller(t *testing.T) {
	cfg := config.DefaultConfig()
	rt, err := buildSpecialistWithStore(cfg, fleet.NewManager(cfg), newSpecialistMemStore(),
		nil, newSpecialistCustodians())
	if err != nil || rt == nil {
		t.Fatal(err)
	}
	caller := mcp.SpecialistCaller{Actor: "token:t-1", TokenID: "t-1", Name: "PagerDuty",
		Kind: "agent", Scopes: []string{"read"}}
	_, err = rt.mcp.SpecialistCall(context.Background(), "specialist_request_remediation",
		caller, "orders", json.RawMessage(`{"investigation_id":`+
			`"44444444-4444-4444-8444-444444444444","remediation_id":"custodian.0123456789abcdef"}`))
	if !errors.Is(err, specialist.ErrScope) {
		t.Fatalf("read caller requesting: %v", err)
	}
	_, err = rt.mcp.SpecialistCall(context.Background(), "specialist_investigation_status",
		caller, "orders", json.RawMessage(`{"investigation_id":`+
			`"44444444-4444-4444-8444-444444444444"}`))
	if !errors.Is(err, specialist.ErrNotFound) {
		t.Fatalf("unknown database for an unrestricted caller: %v", err)
	}
	_, err = rt.mcp.SpecialistCall(context.Background(), "specialist_nope", caller, "orders",
		json.RawMessage(`{}`))
	if !errors.Is(err, specialist.ErrInvalid) {
		t.Fatalf("unknown tool: %v", err)
	}
}

// specialistMemStore is a request store for wiring tests (the PostgreSQL
// store is tested in internal/specialist against a live server).
type specialistMemStore struct{ records []specialist.Record }

func newSpecialistMemStore() *specialistMemStore { return &specialistMemStore{} }

func (m *specialistMemStore) Record(_ context.Context, r specialist.Record) (
	specialist.Record, error) {
	m.records = append(m.records, r)
	return r, nil
}

func (m *specialistMemStore) LiveOpened(context.Context, string) ([]specialist.LiveRef,
	error) {
	return nil, nil
}

func (m *specialistMemStore) MarkTerminal(context.Context, string) error { return nil }

func (m *specialistMemStore) ForInvestigation(context.Context, string, string, string) (
	*specialist.Record, error) {
	return nil, nil
}

func (m *specialistMemStore) HasExternal(context.Context, string, string) (bool, error) {
	return false, nil
}

func (m *specialistMemStore) Recent(context.Context, int) ([]specialist.Record, error) {
	return m.records, nil
}

func (m *specialistMemStore) ClaimOutbound(context.Context, time.Time, time.Duration,
	int) ([]specialist.Record, error) {
	return nil, nil
}

func (m *specialistMemStore) FinishOutbound(context.Context, string, string, string,
	time.Time) error {
	return nil
}

func (m *specialistMemStore) RescheduleOutbound(context.Context, string, time.Time) error {
	return nil
}
