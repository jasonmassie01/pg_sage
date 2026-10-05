package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/ask"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Ask Sage wiring (roadmap phase 3): the adapters between Ask Sage's
// narrow interfaces and the existing paths. A proposal goes only through
// executor.ProposeFindingForApproval (queue, never execute); an
// investigation only through sre.Service.Start with the operator trigger;
// reads through the redacted investigation service and the Trust view.

type fakeFindingProposer struct {
	got    int64
	origin executor.ProposalOrigin
	result executor.FindingProposal
	err    error
}

func (f *fakeFindingProposer) ProposeFindingForApproval(_ context.Context,
	id int64, origin executor.ProposalOrigin) (executor.FindingProposal, error) {
	f.got, f.origin = id, origin
	return f.result, f.err
}

func TestAskProposer_MapsTheExecutorProposal(t *testing.T) {
	pct := -40.0
	fp := &fakeFindingProposer{result: executor.FindingProposal{QueueID: 12, FindingID: 5,
		Created: true, ActionType: "create_index_concurrently", SQL: "CREATE INDEX ...",
		RollbackSQL: "DROP INDEX ...", RollbackClass: "reversible",
		Decision: executor.ActionPolicyDecision{Decision: "queue_approval",
			RiskTier: "moderate", BlockedReason: "approval_required"},
		Prediction: verify.Prediction{Class: "index_create", Method: "hypopg",
			ExpectedChangePct: &pct}}}
	p, err := askProposer{inner: fp}.ProposeFinding(context.Background(), 5, "user:1")
	if err != nil || fp.got != 5 || fp.origin != (executor.ProposalOrigin{
		Via: executor.ProposedViaAskSage, By: "user:1"}) {
		t.Fatalf("propose: %v (got %d, origin %+v)", err, fp.got, fp.origin)
	}
	if p.QueueID != 12 || !p.Created || p.Verdict != "queue_approval" || p.RiskTier != "moderate" ||
		p.Reason != "approval_required" || p.RollbackSQL != "DROP INDEX ..." ||
		p.RollbackClass != "reversible" || p.ActionType != "create_index_concurrently" {
		t.Fatalf("proposal = %+v", p)
	}
	var pred map[string]any
	if json.Unmarshal(p.Prediction, &pred) != nil || pred["expected_change_pct"] != -40.0 {
		t.Fatalf("prediction = %s", p.Prediction)
	}
}

func TestAskProposer_MapsTheExecutorErrors(t *testing.T) {
	cases := map[error]error{
		fmt.Errorf("%w: resolved", executor.ErrNotProposable):         ask.ErrRefused,
		fmt.Errorf("%w: emergency stop", executor.ErrProposalBlocked): ask.ErrBlocked,
		fmt.Errorf("%w", executor.ErrApprovalQueueUnavailable):        ask.ErrUnavailable,
		errors.New("insert failed"):                                   nil,
	}
	for in, want := range cases {
		_, err := askProposer{inner: &fakeFindingProposer{err: in}}.ProposeFinding(
			context.Background(), 5, "ask:user:1")
		if err == nil {
			t.Fatalf("%v: no error", in)
		}
		if want != nil && !errors.Is(err, want) {
			t.Errorf("%v mapped to %v, want %v", in, err, want)
		}
		if !strings.Contains(err.Error(), strings.TrimPrefix(in.Error(), "")) {
			t.Errorf("%v lost its detail: %v", in, err)
		}
	}
	if _, err := (askProposer{}).ProposeFinding(context.Background(), 5, "a"); !errors.Is(err,
		ask.ErrUnavailable) {
		t.Fatalf("no executor: %v", err)
	}
}

type fakeInvestigationService struct {
	trigger sre.Trigger
	inv     sre.Investigation
	created bool
	err     error
	page    sre.Page
	detail  sre.Detail
}

func (f *fakeInvestigationService) Start(_ context.Context, t sre.Trigger) (sre.Investigation,
	bool, error) {
	f.trigger = t
	return f.inv, f.created, f.err
}

func (f *fakeInvestigationService) List(context.Context, sre.ListFilter) (sre.Page, error) {
	return f.page, f.err
}

func (f *fakeInvestigationService) Detail(_ context.Context, id sre.UUID) (sre.Detail, error) {
	if f.err != nil {
		return sre.Detail{}, f.err
	}
	if f.detail.Investigation.ID != id {
		return sre.Detail{}, sre.ErrNotFound
	}
	return f.detail, nil
}

func TestAskStarter_UsesTheOperatorTrigger(t *testing.T) {
	svc := &fakeInvestigationService{inv: sre.Investigation{ID: "9b2e"}, created: true}
	got, err := askStarter{svc: svc}.StartInvestigation(context.Background(),
		ask.StartRequest{Subject: "checkout latency", CaseID: "ask:abc", Actor: "ask:user:1"})
	if err != nil || got.ID != "9b2e" || !got.Created {
		t.Fatalf("start = %+v (%v)", got, err)
	}
	tr := svc.trigger
	if tr.Kind != sre.TriggerOperator || tr.Subject != "checkout latency" ||
		tr.CaseID != "ask:abc" || tr.Actor != "ask:user:1" || tr.IdempotencyKey == "" {
		t.Fatalf("trigger = %+v", tr)
	}
	svc.err = sre.ErrInvalidRequest
	if _, err := (askStarter{svc: svc}).StartInvestigation(context.Background(),
		ask.StartRequest{Subject: "x", CaseID: "c", Actor: "a"}); !errors.Is(err,
		ask.ErrRefused) {
		t.Fatalf("invalid trigger: %v, want ErrRefused", err)
	}
	if _, err := (askStarter{}).StartInvestigation(context.Background(),
		ask.StartRequest{}); !errors.Is(err, ask.ErrUnavailable) {
		t.Fatalf("no investigator: %v", err)
	}
}

func TestAskInvestigations_ReadThroughTheRedactedService(t *testing.T) {
	svc := &fakeInvestigationService{page: sre.Page{Items: []sre.Investigation{{ID: "a1",
		Subject: "lock chain", State: "concluded"}}},
		detail: sre.Detail{Database: "db1", Investigation: sre.Investigation{ID: "a1",
			ProbeCount: 4}}}
	src := askInvestigations{svc: svc}
	list, err := src.ListInvestigations(context.Background(), 5)
	if err != nil || !strings.Contains(string(list), "lock chain") {
		t.Fatalf("list = %s (%v)", list, err)
	}
	one, err := src.Investigation(context.Background(), "a1")
	if err != nil || !strings.Contains(string(one), `"probe_count":4`) {
		t.Fatalf("detail = %s (%v)", one, err)
	}
	if _, err := src.Investigation(context.Background(), "zz"); !errors.Is(err,
		ask.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

type fakeTrustViewer struct {
	view earned.TrustView
	ok   bool
	err  error
	db   string
}

func (f *fakeTrustViewer) TrustView(_ context.Context, db string) (earned.TrustView, bool,
	error) {
	f.db = db
	return f.view, f.ok, f.err
}

func TestAskTrust_ReadsTheDatabasesTrustView(t *testing.T) {
	v := &fakeTrustViewer{ok: true, view: earned.TrustView{Database: "db1",
		Rows: []earned.TrustRow{{Family: "index", Class: "create"}}}}
	raw, err := askTrust{viewer: v, database: "db1"}.Trust(context.Background())
	if err != nil || v.db != "db1" || !strings.Contains(string(raw), `"index"`) {
		t.Fatalf("trust = %s (%v)", raw, err)
	}
	v.ok = false
	if _, err := (askTrust{viewer: v, database: "db1"}).Trust(context.Background()); !errors.Is(
		err, ask.ErrNotFound) {
		t.Fatalf("no ledger: %v", err)
	}
}

func TestAskRegistry_IsProcessWide(t *testing.T) {
	if askServices() == nil || askServices() != askServices() {
		t.Fatal("the Ask Sage registry is not a single process-wide value")
	}
}
