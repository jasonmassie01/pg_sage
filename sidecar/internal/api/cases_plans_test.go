package api

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/changeplan"
)

// ux-03: a finding case lists the plans its finding is a step of
// (plans: [{id, goal}]), read through the plan store.
// No concurrent access tests here: the enrichment fills one request's
// slice; changeplan.TestPlansForFindings_ConcurrentReadersAgree covers reads.

type fakeCasePlans struct {
	got  []int64
	byID map[int64][]changeplan.PlanRef
	err  error
}

func (f *fakeCasePlans) PlansForFindings(_ context.Context, ids []int64) (
	map[int64][]changeplan.PlanRef, error) {
	f.got = ids
	return f.byID, f.err
}

func findingCase(id string) cases.Case {
	return cases.Case{ID: "finding:" + id, SourceType: cases.SourceFindingType,
		SourceIDs: []string{id}}
}

func TestAttachCasePlans_LinksEachFindingCase(t *testing.T) {
	f := &fakeCasePlans{byID: map[int64][]changeplan.PlanRef{
		11: {{ID: 7, Goal: "cut blocks per call"}}}}
	projected := []cases.Case{findingCase("11"), findingCase("12"),
		{ID: "incident:1", SourceType: cases.SourceIncidentType}}
	attachCasePlans(context.Background(), projected, "orders", f, []int{11, 12})
	if !reflect.DeepEqual(f.got, []int64{11, 12}) {
		t.Fatalf("read finding ids %v, want [11 12]", f.got)
	}
	want := []cases.PlanLink{{ID: 7, Goal: "cut blocks per call"}}
	if !reflect.DeepEqual(projected[0].Plans, want) {
		t.Fatalf("case 11 plans = %+v, want %+v", projected[0].Plans, want)
	}
	if projected[1].Plans != nil || projected[2].Plans != nil ||
		projected[0].PlansUnavailable {
		t.Fatalf("other cases changed: %+v", projected[1:])
	}
}

func TestAttachCasePlans_ReadFailureMarksFindingCasesUnavailable(t *testing.T) {
	f := &fakeCasePlans{err: errors.New("relation sage.plan_step does not exist")}
	projected := []cases.Case{findingCase("11"),
		{ID: "incident:1", SourceType: cases.SourceIncidentType}}
	attachCasePlans(context.Background(), projected, "orders", f, []int{11})
	if !projected[0].PlansUnavailable || projected[0].Plans != nil {
		t.Fatalf("case 11 = %+v, want plans_unavailable and no plans", projected[0])
	}
	if projected[1].PlansUnavailable {
		t.Fatal("an incident case has no finding, so no plan read to fail")
	}
}

func TestAttachCasePlans_NoFindingsReadsNothing(t *testing.T) {
	f := &fakeCasePlans{err: errors.New("must not be called")}
	projected := []cases.Case{{ID: "incident:1", SourceType: cases.SourceIncidentType}}
	attachCasePlans(context.Background(), projected, "orders", f, nil)
	if f.got != nil || projected[0].PlansUnavailable {
		t.Fatalf("read %v for no findings; case %+v", f.got, projected[0])
	}
	attachCasePlans(context.Background(), nil, "orders", nil, []int{11})
}
