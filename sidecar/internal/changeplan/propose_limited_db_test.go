package changeplan

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// plan-03b: an agent proposes a plan over MCP. It is stored proposed with
// origin agent and the principal id, after the plan-01a validation, and a
// principal may hold at most limit open plans (the fifth is accepted, the
// sixth refused). A refused proposal stores nothing.

func agentPlan(principal string) Plan {
	p := threeStepPlan()
	p.Origin, p.OriginRef, p.CreatedBy, p.Status = OriginAgent, principal,
		"mcp:token:t1", StatusProposed
	return p
}

func countPlans(t *testing.T, s *Store, ref string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.plan
		WHERE origin_ref = $1`, ref).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestProposeLimited_StoresAProposedAgentPlanUpToTheLimit(t *testing.T) {
	s := NewStore(planPool(t, "propose_limited"))
	ctx := context.Background()
	for i := 1; i <= DefaultOpenPlansPerPrincipal; i++ {
		got, err := s.ProposeLimited(ctx, agentPlan("agp_one"), newFakeResolver(),
			DefaultOpenPlansPerPrincipal)
		if err != nil {
			t.Fatalf("plan %d refused: %v", i, err)
		}
		if got.ID <= 0 || got.Status != StatusProposed || got.Origin != OriginAgent ||
			got.OriginRef != "agp_one" || got.PlanHash == "" || len(got.Steps) != 3 {
			t.Fatalf("plan %d stored as %+v", i, got)
		}
	}
	_, err := s.ProposeLimited(ctx, agentPlan("agp_one"), newFakeResolver(),
		DefaultOpenPlansPerPrincipal)
	var le *OpenPlanLimitError
	if !errors.Is(err, ErrOpenPlanLimit) || !errors.As(err, &le) || le.Open != 5 ||
		le.Limit != 5 {
		t.Fatalf("sixth plan: %v, want ErrOpenPlanLimit with 5 of 5 open", err)
	}
	if n := countPlans(t, s, "agp_one"); n != 5 {
		t.Fatalf("%d plans stored for agp_one, want 5", n)
	}
	// another principal has its own budget
	if _, err := s.ProposeLimited(ctx, agentPlan("agp_two"), newFakeResolver(), 5); err != nil {
		t.Fatalf("another principal refused: %v", err)
	}
}

func TestProposeLimited_ClosedPlansDoNotCount(t *testing.T) {
	s := NewStore(planPool(t, "propose_limited_closed"))
	ctx := context.Background()
	first, err := s.ProposeLimited(ctx, agentPlan("agp_c"), newFakeResolver(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProposeLimited(ctx, agentPlan("agp_c"), newFakeResolver(),
		1); !errors.Is(err, ErrOpenPlanLimit) {
		t.Fatalf("second open plan at limit 1: %v", err)
	}
	if err := s.Reject(ctx, first.ID, "not now", 7); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProposeLimited(ctx, agentPlan("agp_c"), newFakeResolver(), 1); err != nil {
		t.Fatalf("a rejected plan still counted: %v", err)
	}
}

func TestProposeLimited_RefusesBadInputAndStoresNothing(t *testing.T) {
	s := NewStore(planPool(t, "propose_limited_bad"))
	ctx := context.Background()
	unknownFinding := agentPlan("agp_bad")
	unknownFinding.Steps[0].FindingID = ptr(int64(999))
	tooMany := agentPlan("agp_bad")
	for len(tooMany.Steps) <= MaxSteps {
		n := len(tooMany.Steps) + 1
		tooMany.Steps = append(tooMany.Steps, indexStep(n, "idx_x", "c", 11, 101))
	}
	noRef := agentPlan("")
	cases := map[string]struct {
		p     Plan
		limit int
		code  string
	}{
		"finding that does not exist": {unknownFinding, 5, CodeEvidenceUnresolved},
		"51 steps":                    {tooMany, 5, CodeStepCount},
	}
	for name, c := range cases {
		_, err := s.ProposeLimited(ctx, c.p, newFakeResolver(), c.limit)
		var ip *InvalidPlanError
		if !errors.As(err, &ip) || !anyCode(ip.Problems, c.code) {
			t.Errorf("%s: %v, want an invalid plan with %s", name, err, c.code)
		}
	}
	for name, limit := range map[string]int{"zero limit": 0, "negative limit": -1} {
		if _, err := s.ProposeLimited(ctx, agentPlan("agp_bad"), newFakeResolver(),
			limit); err == nil || errors.Is(err, ErrOpenPlanLimit) {
			t.Errorf("%s: %v, want an input error", name, err)
		}
	}
	if _, err := s.ProposeLimited(ctx, noRef, newFakeResolver(), 5); err == nil {
		t.Error("a plan with no principal was stored")
	}
	if n := countPlans(t, s, "agp_bad"); n != 0 {
		t.Fatalf("%d refused plans stored", n)
	}
}

func anyCode(ps []Problem, code string) bool {
	for _, p := range ps {
		if p.Code == code {
			return true
		}
	}
	return false
}

func TestProposeLimited_ConcurrentProposalsKeepTheLimit(t *testing.T) {
	s := NewStore(planPool(t, "propose_limited_race"))
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.ProposeLimited(context.Background(), agentPlan("agp_race"),
				newFakeResolver(), 3)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	ok, limited := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrOpenPlanLimit):
			limited++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 3 || limited != 5 || countPlans(t, s, "agp_race") != 3 {
		t.Fatalf("%d stored, %d limited, want 3 and 5", ok, limited)
	}
}

func TestProposalNotifier_HearsProposedPlansOnly(t *testing.T) {
	pool := planPool(t, "plan_notifier")
	s := NewStore(pool)
	var mu sync.Mutex
	var heard []int64
	unset := SetProposalNotifier(pool, func(_ context.Context, p Plan) {
		mu.Lock()
		defer mu.Unlock()
		heard = append(heard, p.ID)
	})
	proposed := mustCreate(t, s, threeStepPlan())
	draft := threeStepPlan()
	draft.Status = StatusDraft
	mustCreate(t, s, draft)
	limited, err := s.ProposeLimited(context.Background(), agentPlan("agp_n"),
		newFakeResolver(), 5)
	if err != nil {
		t.Fatal(err)
	}
	unset()
	mustCreate(t, s, threeStepPlan())
	mu.Lock()
	defer mu.Unlock()
	if len(heard) != 2 || heard[0] != proposed.ID || heard[1] != limited.ID {
		t.Fatalf("notifier heard %v, want the two proposed plans %d and %d", heard,
			proposed.ID, limited.ID)
	}
	unset() // a second unset is harmless
	SetProposalNotifier(nil, nil)()
}
