package changeplan

import (
	"encoding/json"
	"regexp"
	"testing"
	"time"
)

// plan-01a: the plan hash binds an approval to every hashed field and to
// nothing else. No concurrent-access test: PlanHash is a pure function.

var hexHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestPlanHash_IsStableAndHex(t *testing.T) {
	a, b := PlanHash(threeStepPlan()), PlanHash(threeStepPlan())
	if a != b || !hexHash.MatchString(a) {
		t.Fatalf("hashes %q and %q, want one 64-hex value", a, b)
	}
}

// hashedEdits each change one hashed field of threeStepPlan.
var hashedEdits = map[string]func(*Plan){
	"goal":            func(p *Plan) { p.Goal += "!" },
	"scope object":    func(p *Plan) { p.Scope.Objects = append(p.Scope.Objects, "public.x") },
	"scope queryid":   func(p *Plan) { p.Scope.QueryIDs = []int64{101} },
	"origin":          func(p *Plan) { p.Origin = OriginModel },
	"pack hash":       func(p *Plan) { p.PackHash = "" },
	"sql":             func(p *Plan) { p.Steps[0].ProposedSQL += " WHERE id > 0" },
	"rollback":        func(p *Plan) { p.Steps[1].RollbackSQL = "DROP INDEX public.idx_b" },
	"action type":     func(p *Plan) { p.Steps[0].ActionType = "replace_index" },
	"kind":            func(p *Plan) { p.Steps[0].Kind = KindOperator },
	"setting":         func(p *Plan) { p.Steps[2].Setting = "seq_page_cost" },
	"finding":         func(p *Plan) { p.Steps[0].FindingID = ptr(int64(12)) },
	"finding cleared": func(p *Plan) { p.Steps[0].FindingID = nil },
	"evidence id":     func(p *Plan) { p.Steps[2].EvidenceIDs = []string{"setting/x"} },
	"target":          func(p *Plan) { p.Steps[0].TargetQueryIDs = []int64{102} },
	"metric":          func(p *Plan) { p.Steps[0].Metric = MetricTimePerCall },
	"direction":       func(p *Plan) { p.Steps[0].ExpectedDirection = DirectionUp },
	"expected pct":    func(p *Plan) { p.Steps[0].ExpectedPct = ptr(40.5) },
	"pct cleared":     func(p *Plan) { p.Steps[0].ExpectedPct = nil },
	"source":          func(p *Plan) { p.Steps[0].ExpectedSource = SourceClone },
	"guardrail pct":   func(p *Plan) { p.Steps[0].Guardrails[0].MaxRegressPct = 11 },
	"guardrail added": func(p *Plan) {
		p.Steps[1].Guardrails = append(p.Steps[1].Guardrails,
			Guardrail{Metric: MetricWALPerTuple, MaxRegressPct: 5})
	},
	"min calls":       func(p *Plan) { p.Steps[0].MinEvidence.MinCalls = 101 },
	"min samples":     func(p *Plan) { p.Steps[0].MinEvidence.MinSamples = 31 },
	"deadline":        func(p *Plan) { p.Steps[0].DeadlineMinutes = 4321 },
	"checkpoint":      func(p *Plan) { p.Steps[0].Checkpoint = true },
	"on improved":     func(p *Plan) { p.Steps[0].OnImproved = ActionHaltAsk },
	"on no change":    func(p *Plan) { p.Steps[0].OnNoChange = ActionRevertStep },
	"on regressed":    func(p *Plan) { p.Steps[1].OnRegressed = ActionRevertToCheckpoint },
	"on inconclusive": func(p *Plan) { p.Steps[1].OnInconclusive = ActionKeep },
	"on unverifiable": func(p *Plan) { p.Steps[2].OnUnverifiable = ActionKeep },
	"on expired":      func(p *Plan) { p.Steps[2].OnExpired = ActionRevertStep },
	"ordinal":         func(p *Plan) { p.Steps[2].Ordinal = 4 },
	"edge removed":    func(p *Plan) { p.Edges = p.Edges[:1] },
	"edge added":      func(p *Plan) { p.Edges = append(p.Edges, Edge{From: 1, To: 2}) },
	"edge reversed":   func(p *Plan) { p.Edges[0] = Edge{From: 3, To: 1} },
	"step removed":    func(p *Plan) { p.Steps = p.Steps[:2] },
}

func TestPlanHash_EveryHashedFieldChangesIt(t *testing.T) {
	base := PlanHash(threeStepPlan())
	seen := map[string]string{base: "base"}
	for name, edit := range hashedEdits {
		p := threeStepPlan()
		edit(&p)
		h := PlanHash(p)
		if h == base {
			t.Errorf("changing the %s left the hash unchanged", name)
		}
		if other, dup := seen[h]; dup && other != "base" {
			t.Errorf("changing the %s and the %s give one hash", name, other)
		}
		seen[h] = name
	}
}

func TestPlanHash_RuntimeAndBookkeepingDoNotChangeIt(t *testing.T) {
	base := PlanHash(threeStepPlan())
	now := time.Now()
	edits := map[string]func(*Plan){
		"state":        func(p *Plan) { p.Steps[0].Runtime.State = StateKept },
		"verdict":      func(p *Plan) { p.Steps[0].Runtime.Verdict = OutcomeRegressed },
		"verdict at":   func(p *Plan) { p.Steps[0].Runtime.VerdictAt = &now },
		"queue id":     func(p *Plan) { p.Steps[1].Runtime.QueueID = ptr(int64(42)) },
		"action log":   func(p *Plan) { p.Steps[1].Runtime.ActionLogID = ptr(int64(43)) },
		"decision":     func(p *Plan) { p.Steps[1].Runtime.DecisionID = ptr(int64(44)) },
		"state reason": func(p *Plan) { p.Steps[2].Runtime.StateReason = "halted" },
		"step id":      func(p *Plan) { p.Steps[2].ID = 77 },
		"plan id":      func(p *Plan) { p.ID = 9 },
		"status":       func(p *Plan) { p.Status = StatusRunning },
		"stored hash":  func(p *Plan) { p.PlanHash = "x" },
		"updated at":   func(p *Plan) { p.UpdatedAt = now },
		"step order in the slice": func(p *Plan) {
			p.Steps[0], p.Steps[2] = p.Steps[2], p.Steps[0]
		},
		"edge order": func(p *Plan) { p.Edges[0], p.Edges[1] = p.Edges[1], p.Edges[0] },
		"scope order": func(p *Plan) {
			p.Scope.QueryIDs = []int64{102, 101}
		},
	}
	for name, edit := range edits {
		p := threeStepPlan()
		edit(&p)
		if h := PlanHash(p); h != base {
			t.Errorf("changing the %s changed the hash", name)
		}
	}
}

func TestPlanHash_JSONFieldOrderDoesNotMatter(t *testing.T) {
	a := `{"goal":"g","origin":"person","status":"proposed","created_by":"a",
	  "steps":[{"ordinal":1,"kind":"action","metric":"time_per_call",
	  "expected_direction":"down","expected_source":"hypopg","deadline_minutes":5,
	  "min_evidence":{"min_calls":3,"min_samples":4},"target_queryids":[1]}],
	  "edges":[]}`
	b := `{"edges":[],"steps":[{"target_queryids":[1],"min_evidence":{"min_samples":4,
	  "min_calls":3},"deadline_minutes":5,"expected_source":"hypopg",
	  "expected_direction":"down","metric":"time_per_call","kind":"action","ordinal":1}],
	  "created_by":"a","status":"proposed","origin":"person","goal":"g"}`
	var pa, pb Plan
	if err := json.Unmarshal([]byte(a), &pa); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(b), &pb); err != nil {
		t.Fatal(err)
	}
	if PlanHash(pa) != PlanHash(pb) {
		t.Fatal("the same plan in another JSON key order hashed differently")
	}
}

func TestPlanHash_EmptyPlanIsDefinedAndDistinct(t *testing.T) {
	empty := PlanHash(Plan{})
	if !hexHash.MatchString(empty) || empty != PlanHash(Plan{Steps: []Step{}, Edges: []Edge{}}) {
		t.Fatalf("empty plan hash %q, want one defined 64-hex value for nil and empty", empty)
	}
	for _, p := range []Plan{threeStepPlan(), {Goal: "g"}, {Steps: []Step{{}}},
		{Edges: []Edge{{}}}} {
		if PlanHash(p) == empty {
			t.Fatalf("plan %+v hashes like the empty plan", p)
		}
	}
}

// A NUL inside one field cannot shift bytes into the next field and
// collide with another plan.
func TestPlanHash_FieldsCannotBleedIntoEachOther(t *testing.T) {
	a, b := threeStepPlan(), threeStepPlan()
	a.Goal, a.Steps[0].ProposedSQL = "ab", "c"
	b.Goal, b.Steps[0].ProposedSQL = "a", "bc"
	if PlanHash(a) == PlanHash(b) {
		t.Fatal("moving bytes between fields kept the hash")
	}
	c, d := threeStepPlan(), threeStepPlan()
	c.Scope.Objects = []string{"a\x00b"}
	d.Scope.Objects = []string{"a", "b"}
	if PlanHash(c) == PlanHash(d) {
		t.Fatal("a NUL inside a scope object collided with two objects")
	}
}
