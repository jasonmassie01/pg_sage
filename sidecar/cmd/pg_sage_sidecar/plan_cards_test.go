package main

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/notify"
)

// plan-03b: a plan proposed over MCP is stored proposed with the
// principal id and sends one plan card, whose buttons are what chat may
// approve (D7). No concurrent access test here: the open-plan limit races
// in internal/changeplan (TestProposeLimited_ConcurrentProposalsKeepTheLimit).

func indexPlanStep(ordinal int, finding int64) changeplan.Step {
	return changeplan.Step{Ordinal: ordinal, Kind: changeplan.KindAction,
		ActionType:  "create_index_concurrently",
		ProposedSQL: "CREATE INDEX CONCURRENTLY idx_pc ON public.orders (customer_id)",
		RollbackSQL: "DROP INDEX CONCURRENTLY public.idx_pc", FindingID: &finding,
		TargetQueryIDs: []int64{101}, Metric: changeplan.MetricBlocksPerCall,
		ExpectedDirection: changeplan.DirectionDown,
		// A fresh database has no calls: the step is declared unverifiable.
		ExpectedSource: changeplan.SourceNone, DeadlineMinutes: 60,
		OnUnverifiable: changeplan.ActionKeep}
}

func terminatePlanStep(ordinal int, finding int64) changeplan.Step {
	return changeplan.Step{Ordinal: ordinal, Kind: changeplan.KindAction,
		ActionType: "terminate_backend", ProposedSQL: "SELECT pg_terminate_backend(4242)",
		FindingID: &finding, TargetQueryIDs: []int64{101},
		Metric: changeplan.MetricTimePerCall, ExpectedDirection: changeplan.DirectionDown,
		ExpectedSource: changeplan.SourceNone, DeadlineMinutes: 60,
		OnUnverifiable: changeplan.ActionKeep}
}

func detailOf(steps ...changeplan.Step) changeplan.Detail {
	p := changeplan.Plan{ID: 7, Goal: "cut blocks per call", Origin: changeplan.OriginAgent,
		CreatedBy: "mcp:token:t1", Status: changeplan.StatusProposed, Steps: steps}
	d := changeplan.Detail{Plan: p}
	for i := range p.Steps {
		p.Steps[i].ID = int64(70 + i)
		d.Coverage = append(d.Coverage, changeplan.StepCoverage{StepID: p.Steps[i].ID,
			Ordinal: p.Steps[i].Ordinal})
	}
	return d
}

func TestPlanCardRefOffersWhatChatMayApprove(t *testing.T) {
	expires := time.Now().Add(time.Hour)
	all := planCardRef("orders", detailOf(indexPlanStep(1, 11), indexPlanStep(2, 11)),
		expires)
	if !slices.Equal(all.Offered, []chatops.PlanChoice{chatops.PlanApproveAll,
		chatops.PlanApproveNext, chatops.PlanReject}) || all.UpTo != 0 || all.Steps != 2 ||
		all.PlanID != 7 || all.Database != "orders" || len(all.PlanHash) != 64 {
		t.Fatalf("two moderate steps: %+v", all)
	}
	upTo := planCardRef("orders", detailOf(indexPlanStep(1, 11), indexPlanStep(2, 11),
		terminatePlanStep(3, 11)), expires)
	if !slices.Equal(upTo.Offered, []chatops.PlanChoice{chatops.PlanApproveUpTo,
		chatops.PlanApproveNext, chatops.PlanReject}) || upTo.UpTo != 2 {
		t.Fatalf("high tier at 3: %+v", upTo)
	}
	none := planCardRef("orders", detailOf(terminatePlanStep(1, 11)), expires)
	if !slices.Equal(none.Offered, []chatops.PlanChoice{chatops.PlanReject}) {
		t.Fatalf("high tier at 1: %+v", none)
	}
	if empty := planCardRef("orders", changeplan.Detail{}, expires); !slices.Equal(
		empty.Offered, []chatops.PlanChoice{chatops.PlanReject}) {
		t.Fatalf("an empty plan offers %v", empty.Offered)
	}
}

func TestPlanCardBodySummarizesGoalStepsRiskAndLink(t *testing.T) {
	body := planCardBody("orders", detailOf(indexPlanStep(1, 11), terminatePlanStep(2, 11)))
	for _, want := range []string{"Plan #7 on orders", "2 steps", "Goal: cut blocks per call",
		"Step 1 (action, risk moderate): blocks_per_call expected down",
		"Step 2 (action, risk high)", "Risk: highest step tier high.",
		"step 2 onward needs the pg_sage app", "pg_sage_sidecar plan show 7 --database orders"} {
		if !strings.Contains(body, want) {
			t.Errorf("card body lacks %q:\n%s", want, body)
		}
	}
}

// planCards returns the plan ids of the plan cards sent so far.
func (s *cardSender) planCards() []notify.PlanCardRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	var refs []notify.PlanCardRef
	for _, evt := range s.sent {
		if ref, ok := notify.PlanCardOf(evt); ok && evt.Type == "approval_needed" {
			refs = append(refs, ref)
		}
	}
	return refs
}

// planRig is the card rig with plan cards registered, a finding to cite
// and the MCP plan backend over the fleet of its one database.
func planRig(t *testing.T) (*cardRig, plansMCPBackend, int64) {
	t.Helper()
	r := newCardRig(t, &cardRules{})
	r.rt.registerPlanCards()
	var finding int64
	if err := r.pool.QueryRow(r.ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommendation, status)
		VALUES ('missing_index', 'warning', 'table', 'public.orders', 'Missing index',
		'{}', 'Create an index', 'open') RETURNING id`).Scan(&finding); err != nil {
		t.Fatal(err)
	}
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "orders", Pool: r.pool,
		Config: config.DatabaseConfig{Name: "orders"}, Status: &fleet.InstanceStatus{}})
	return r, plansMCPBackend{manager: mgr}, finding
}

func TestPlanCardMCPProposeSendsOneCard(t *testing.T) {
	r, backend, finding := planRig(t)
	p := changeplan.Plan{Goal: "cut blocks per call on orders", Origin: changeplan.OriginAgent,
		OriginRef: "agp_alpha", CreatedBy: "mcp:token:t1", Steps: []changeplan.Step{
			indexPlanStep(1, finding)}}
	stored, err := backend.ProposePlan(r.ctx, "orders", p, 5)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if stored.Status != changeplan.StatusProposed || stored.OriginRef != "agp_alpha" ||
		stored.Origin != changeplan.OriginAgent {
		t.Fatalf("stored %+v", stored)
	}
	cards := r.sender.planCards()
	if len(cards) != 1 || cards[0].PlanID != stored.ID || cards[0].PlanHash !=
		stored.PlanHash || !slices.Equal(cards[0].Offered, []chatops.PlanChoice{
		chatops.PlanApproveAll, chatops.PlanReject}) {
		t.Fatalf("plan cards %+v, want one for plan %d", cards, stored.ID)
	}
	if !strings.Contains(r.sender.sent[len(r.sender.sent)-1].Body, "agp_alpha") &&
		!strings.Contains(r.sender.sent[len(r.sender.sent)-1].Body, "mcp:token:t1") {
		t.Fatalf("card body does not say who proposed: %s", r.sender.sent[0].Body)
	}
	readAndRefuse(t, r, backend, p, stored, finding)
}

func readAndRefuse(t *testing.T, r *cardRig, backend plansMCPBackend, p, stored changeplan.Plan,
	finding int64) {
	t.Helper()
	listed, err := backend.ListPlans(r.ctx, "", changeplan.ListFilter{Limit: 10})
	if err != nil || len(listed) != 1 || listed[0].ID != stored.ID {
		t.Fatalf("list with the only database: %+v %v", listed, err)
	}
	got, err := backend.GetPlan(r.ctx, "orders", stored.ID)
	if err != nil || got.Plan.ID != stored.ID || len(got.Coverage) != 1 {
		t.Fatalf("get: %+v %v", got, err)
	}
	bad := p
	bad.Steps = []changeplan.Step{indexPlanStep(1, finding+1000)}
	var ip *changeplan.InvalidPlanError
	if _, err := backend.ProposePlan(r.ctx, "orders", bad, 5); !errors.As(err, &ip) {
		t.Fatalf("a plan citing a missing finding: %v", err)
	}
	if len(r.sender.planCards()) != 1 {
		t.Fatal("a refused proposal sent a card")
	}
	if _, err := backend.GetPlan(r.ctx, "billing", 1); !errors.Is(err,
		changeplan.ErrNotFound) {
		t.Fatalf("unknown database: %v", err)
	}
	if _, err := (plansMCPBackend{}).ListPlans(r.ctx, "orders",
		changeplan.ListFilter{Limit: 1}); !errors.Is(err, changeplan.ErrNotFound) {
		t.Fatalf("no fleet: %v", err)
	}
}
