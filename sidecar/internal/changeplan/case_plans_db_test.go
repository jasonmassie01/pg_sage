package changeplan

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// ux-03: PlansForFindings is how a case finds the plans its findings are
// steps of (sage.plan_step.finding_id), for "Part of plan: <goal>".

func TestPlansForFindings_MapsEachFindingToItsLivePlans(t *testing.T) {
	pool := planPool(t, "case_plans")
	s := NewStore(pool)
	ctx := context.Background()
	a := mustCreate(t, s, threeStepPlan()) // findings 11 and 12
	b := threeStepPlan()
	b.Goal = "drop the redundant index"
	b.Steps = b.Steps[:1] // finding 11 only
	b.Edges = nil
	second := mustCreate(t, s, b)
	got, err := s.PlansForFindings(ctx, []int64{11, 12, 13})
	if err != nil {
		t.Fatalf("PlansForFindings: %v", err)
	}
	want := map[int64][]PlanRef{
		11: {{ID: second.ID, Goal: "drop the redundant index"},
			{ID: a.ID, Goal: "cut blocks per call on the order lookups"}},
		12: {{ID: a.ID, Goal: "cut blocks per call on the order lookups"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PlansForFindings = %+v, want %+v", got, want)
	}
}

func TestPlansForFindings_LeavesOutDeadPlans(t *testing.T) {
	pool := planPool(t, "case_plans_dead")
	s := NewStore(pool)
	ctx := context.Background()
	for _, st := range []Status{StatusDraft, StatusRejected, StatusVoided, StatusCancelled,
		StatusHalted, StatusCompleted} {
		p := mustCreate(t, s, threeStepPlan())
		if _, err := pool.Exec(ctx, `UPDATE sage.plan SET status = $2 WHERE id = $1`, p.ID,
			string(st)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.PlansForFindings(ctx, []int64{11})
	if err != nil {
		t.Fatalf("PlansForFindings: %v", err)
	}
	if len(got[11]) != 2 {
		t.Fatalf("finding 11 plans = %+v, want only the halted and completed ones", got[11])
	}
}

func TestPlansForFindings_CapsEachFinding(t *testing.T) {
	pool := planPool(t, "case_plans_cap")
	s := NewStore(pool)
	var newest int64
	for range MaxCasePlans + 2 {
		newest = mustCreate(t, s, threeStepPlan()).ID
	}
	got, err := s.PlansForFindings(context.Background(), []int64{11})
	if err != nil {
		t.Fatalf("PlansForFindings: %v", err)
	}
	if len(got[11]) != MaxCasePlans || got[11][0].ID != newest {
		t.Fatalf("finding 11 plans = %+v, want the %d newest, newest first", got[11],
			MaxCasePlans)
	}
}

func TestPlansForFindings_NoFindingsReadsNothing(t *testing.T) {
	// A nil pool would panic on any query: no ids must not query.
	s := NewStore(nil)
	for _, ids := range [][]int64{nil, {}} {
		got, err := s.PlansForFindings(context.Background(), ids)
		if err != nil || len(got) != 0 {
			t.Fatalf("PlansForFindings(%v) = %v, %v; want empty, nil", ids, got, err)
		}
	}
}

func TestPlansForFindings_ReadErrorNamesTheRead(t *testing.T) {
	pool := planPool(t, "case_plans_err")
	s := NewStore(pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP TABLE sage.plan_step CASCADE`); err != nil {
		t.Fatal(err)
	}
	_, err := s.PlansForFindings(ctx, []int64{11})
	if err == nil || !strings.Contains(err.Error(), "plans for findings") {
		t.Fatalf("PlansForFindings on a dropped table = %v, want a named read error", err)
	}
}

func TestPlansForFindings_ConcurrentReadersAgree(t *testing.T) {
	pool := planPool(t, "case_plans_conc")
	s := NewStore(pool)
	p := mustCreate(t, s, threeStepPlan())
	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.PlansForFindings(context.Background(), []int64{12})
			if err != nil || len(got[12]) != 1 || got[12][0].ID != p.ID {
				errs <- "concurrent read disagreed"
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
}
