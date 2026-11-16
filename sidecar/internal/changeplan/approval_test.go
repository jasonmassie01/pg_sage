package changeplan

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// plan-03a: approve all, up to step N, or step by step. The coverage rule
// is a pure function of the plan, its live approval and its step
// approvals. No concurrent access test here: Coverage and the decision
// checks are pure (the store's concurrency is in approval_db_test.go).

var userSettings = SettingContexts{"random_page_cost": "user"}

// hashedPlan is threeStepPlan with step ids and its plan hash.
func hashedPlan() Plan {
	p := threeStepPlan()
	for i := range p.Steps {
		p.Steps[i] = p.Steps[i].WithDefaults()
		p.Steps[i].ID = int64(100 + i + 1)
	}
	p.ID = 5
	p.PlanHash = PlanHash(p)
	return p
}

func liveAll(p Plan) *Approval {
	return &Approval{ID: 1, PlanID: p.ID, PlanHash: p.PlanHash, ApproverUserID: 7,
		Mode: ModeAll}
}

func decisionCode(err error) string {
	var de *DecisionError
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

func TestDecisionCheck_RefusesEachBadInputWithItsCode(t *testing.T) {
	hash := strings.Repeat("a", 64)
	cases := map[string]struct {
		d    Decision
		code string
	}{
		"missing hash": {Decision{Mode: ModeAll, Via: ViaAPI}, CodeHashRequired},
		"blank hash":   {Decision{PlanHash: "  ", Mode: ModeAll, Via: ViaAPI}, CodeHashRequired},
		"unknown mode": {Decision{PlanHash: hash, Mode: "most", Via: ViaAPI}, CodeInvalidMode},
		"empty mode":   {Decision{PlanHash: hash, Via: ViaAPI}, CodeInvalidMode},
		"up_to zero":   {Decision{PlanHash: hash, Mode: ModeUpTo, Via: ViaAPI}, CodeInvalidUpTo},
		"up_to negative": {Decision{PlanHash: hash, Mode: ModeUpTo, UpTo: -1, Via: ViaAPI},
			CodeInvalidUpTo},
		"up_to on all": {Decision{PlanHash: hash, Mode: ModeAll, UpTo: 2, Via: ViaAPI},
			CodeInvalidUpTo},
		"up_to past the cap": {Decision{PlanHash: hash, Mode: ModeUpTo, UpTo: 51, Via: ViaAPI},
			CodeInvalidUpTo},
		"unknown channel": {Decision{PlanHash: hash, Mode: ModeAll, Via: "fax"}, CodeInvalidVia},
		"no channel":      {Decision{PlanHash: hash, Mode: ModeAll}, CodeInvalidVia},
	}
	for name, c := range cases {
		err := c.d.Check()
		if !errors.Is(err, ErrInvalidDecision) || decisionCode(err) != c.code {
			t.Errorf("%s: %v (code %q), want %s", name, err, decisionCode(err), c.code)
		}
	}
	for _, ok := range []Decision{{PlanHash: hash, Mode: ModeAll, Via: ViaUI},
		{PlanHash: hash, Mode: ModeUpTo, UpTo: 1, Via: ViaCLI},
		{PlanHash: hash, Mode: ModeStep, Via: ViaChat}} {
		if err := ok.Check(); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
}

// Boundary: up_to equal to the step count is all; past it is refused.
func TestDecisionForPlan_UpToTheStepCountIsAll(t *testing.T) {
	hash := strings.Repeat("a", 64)
	got, err := Decision{PlanHash: hash, Mode: ModeUpTo, UpTo: 3, Via: ViaAPI}.forPlan(3)
	if err != nil || got.Mode != ModeAll || got.UpTo != 0 {
		t.Fatalf("up_to 3 of 3 = %+v (%v), want all", got, err)
	}
	got, err = Decision{PlanHash: hash, Mode: ModeUpTo, UpTo: 2, Via: ViaAPI}.forPlan(3)
	if err != nil || got.Mode != ModeUpTo || got.UpTo != 2 {
		t.Fatalf("up_to 2 of 3 = %+v (%v), want up_to 2", got, err)
	}
	_, err = Decision{PlanHash: hash, Mode: ModeUpTo, UpTo: 4, Via: ViaAPI}.forPlan(3)
	if decisionCode(err) != CodeInvalidUpTo || !strings.Contains(err.Error(), "3 steps") {
		t.Fatalf("up_to 4 of 3: %v, want invalid_up_to naming the 3 steps", err)
	}
}

func coveredOrdinals(cov []StepCoverage) []int {
	var out []int
	for _, c := range cov {
		if c.Covered {
			out = append(out, c.Ordinal)
		}
	}
	return out
}

func TestCoverage_EachMode(t *testing.T) {
	p := hashedPlan()
	all := Coverage(p, liveAll(p), nil, userSettings)
	if got := coveredOrdinals(all); len(got) != 3 || all[0].By != CoveredByPlan ||
		all[0].StepID != 101 {
		t.Fatalf("all covers %v (%+v), want 1-3 by the plan", got, all)
	}
	upTo := liveAll(p)
	upTo.Mode, upTo.UpTo = ModeUpTo, ptr(2)
	cov := Coverage(p, upTo, nil, userSettings)
	if got := coveredOrdinals(cov); len(got) != 2 || got[0] != 1 || got[1] != 2 ||
		!strings.Contains(cov[2].Reason, "up to step 2") {
		t.Fatalf("up_to 2 covers %v (%+v), want 1-2 and why 3 is not", got, cov)
	}
	step := liveAll(p)
	step.Mode = ModeStep
	cov = Coverage(p, step, nil, userSettings)
	if got := coveredOrdinals(cov); len(got) != 0 ||
		!strings.Contains(cov[0].Reason, "step by step") {
		t.Fatalf("step covers %v (%+v), want nothing", got, cov)
	}
	approvals := []StepApproval{{ID: 1, PlanID: p.ID, StepID: 101, Ordinal: 1,
		PlanHash: p.PlanHash, ApproverUserID: 9, Via: ViaCLI}}
	cov = Coverage(p, step, approvals, userSettings)
	if got := coveredOrdinals(cov); len(got) != 1 || got[0] != 1 || cov[0].By != CoveredByStep {
		t.Fatalf("step with step 1 approved covers %v (%+v), want only 1 by its step", got,
			cov)
	}
}

// Nil/empty: no approval covers nothing; neither does an approval of
// another version of the plan, a voided step approval or a step approval
// at an old hash.
func TestCoverage_NothingWithoutACurrentApproval(t *testing.T) {
	p := hashedPlan()
	for name, live := range map[string]*Approval{"none": nil,
		"stale": {ID: 1, PlanID: p.ID, PlanHash: strings.Repeat("b", 64), Mode: ModeAll}} {
		cov := Coverage(p, live, nil, userSettings)
		if len(cov) != 3 || len(coveredOrdinals(cov)) != 0 || cov[0].Reason == "" {
			t.Errorf("%s: %+v, want three uncovered steps with a reason", name, cov)
		}
	}
	step := liveAll(p)
	step.Mode = ModeStep
	voided := time.Now()
	stale := []StepApproval{
		{StepID: 101, Ordinal: 1, PlanHash: strings.Repeat("b", 64), Via: ViaAPI},
		{StepID: 102, Ordinal: 2, PlanHash: p.PlanHash, Via: ViaAPI, VoidedAt: &voided,
			VoidReason: VoidEdited}}
	if got := coveredOrdinals(Coverage(p, step, stale, userSettings)); len(got) != 0 {
		t.Fatalf("stale and voided step approvals cover %v", got)
	}
	if cov := Coverage(Plan{}, nil, nil, nil); len(cov) != 0 {
		t.Fatalf("a plan with no steps has coverage %+v", cov)
	}
}

// Boundary: a never-automatic step at ordinal 1 under all still needs its
// own approval; with it, it is covered by its step.
func TestCoverage_NeverAutomaticFirstStepNeedsItsOwnApproval(t *testing.T) {
	p := hashedPlan()
	p.Steps[0] = settingStep(1).WithDefaults()
	p.Steps[0].ID = 101
	p.Steps[0].Setting = "shared_buffers"
	p.Steps[0].ProposedSQL = "ALTER SYSTEM SET shared_buffers = '256MB'"
	p.PlanHash = PlanHash(p)
	settings := SettingContexts{"shared_buffers": "postmaster", "random_page_cost": "user"}
	cov := Coverage(p, liveAll(p), nil, settings)
	if cov[0].Covered || !cov[0].NeedsOwnApproval ||
		!strings.HasPrefix(cov[0].Reason, ReasonRestartBound) || !cov[1].Covered {
		t.Fatalf("coverage %+v, want step 1 needing its own approval, step 2 covered", cov)
	}
	own := []StepApproval{{StepID: 101, Ordinal: 1, PlanHash: p.PlanHash, Via: ViaUI,
		ApproverUserID: 9}}
	cov = Coverage(p, liveAll(p), own, settings)
	if !cov[0].Covered || cov[0].By != CoveredByStep || !cov[0].NeedsOwnApproval {
		t.Fatalf("coverage with its own approval %+v, want covered by its step", cov[0])
	}
}

// A check step changes nothing: an approved plan covers it in every mode.
func TestCoverage_CheckStepIsCoveredByAnyApproval(t *testing.T) {
	p := hashedPlan()
	p.Steps[2] = Step{ID: 103, Ordinal: 3, Kind: KindCheck, Metric: MetricBlocksPerCall,
		TargetQueryIDs: []int64{101}, ExpectedDirection: DirectionNone,
		ExpectedSource: SourceNone, DeadlineMinutes: 60}.WithDefaults()
	p.PlanHash = PlanHash(p)
	step := liveAll(p)
	step.Mode = ModeStep
	cov := Coverage(p, step, nil, userSettings)
	if !cov[2].Covered || cov[0].Covered {
		t.Fatalf("coverage %+v, want only the check step covered", cov)
	}
}

// The person transitions plan-03a adds, and nothing else out of
// awaiting_approval, failed or halted.
func TestPersonTransitions_AreLegal(t *testing.T) {
	for _, m := range [][2]StepState{{StateAwaitingApproval, StatePending},
		{StateFailed, StateSkipped}, {StateHalted, StateKept}} {
		if !legal(m[0], m[1]) {
			t.Errorf("%s -> %s is not legal", m[0], m[1])
		}
	}
	for _, m := range [][2]StepState{{StateFailed, StateKept}, {StateHalted, StatePending},
		{StateFailed, StatePending}, {StateAwaitingApproval, StateKept}} {
		if legal(m[0], m[1]) {
			t.Errorf("%s -> %s is legal", m[0], m[1])
		}
	}
}

// A step a person resumed past no longer halts the plan.
func TestHaltReason_IgnoresAResumedStep(t *testing.T) {
	s := indexStep(1, "idx_a", "a", 11, 101)
	s.Runtime = StepRuntime{State: StateKept, Verdict: OutcomeRegressed}
	if haltReason(s) == "" {
		t.Fatal("a regressed step does not halt")
	}
	now := time.Now()
	s.Runtime.ResumedAt = &now
	if why := haltReason(s); why != "" {
		t.Fatalf("a resumed step halts: %s", why)
	}
}
