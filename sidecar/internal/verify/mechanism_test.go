package verify

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// The mechanism gate (lab-01b): an index step is credited only for the
// targets whose plan changed at the step and now names the index. Pure
// functions over typed inputs: no shared state, so no concurrent-access
// test applies beyond the -race run of the package.

var mechStep = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

const mechCycle = time.Minute

func indexPlan(name string) []byte {
	return []byte(`[{"Plan":{"Node Type":"Index Scan","Index Name":"` + name +
		`","Relation Name":"orders","Actual Rows":3,"Actual Loops":1}}]`)
}

func estimatedIndexPlan(name string) []byte {
	return []byte(`[{"Plan":{"Node Type":"Index Scan","Index Name":"` + name +
		`","Relation Name":"orders","Plan Rows":3}}]`)
}

var seqPlan = []byte(`[{"Plan":{"Node Type":"Seq Scan","Relation Name":"orders",
	"Filter":"(orders_status_idx = 1)","Actual Rows":3}}]`)

// flip is a fingerprint history: "A" up to the step, then "B" from after.
func flip(after time.Duration) []PlanSample {
	return []PlanSample{{At: mechStep.Add(-2 * mechCycle), Hash: "A"},
		{At: mechStep, Hash: "A"}, {At: mechStep.Add(after), Hash: "B"},
		{At: mechStep.Add(after + mechCycle), Hash: "B"}}
}

func mechInput(class string) MechanismInput {
	return MechanismInput{Class: class, Index: "orders_status_idx", Targets: []int64{1, 2},
		ExecutedAt: mechStep, Cycle: mechCycle,
		PlanHashes: map[int64][]PlanSample{1: flip(30 * time.Second),
			2: flip(30 * time.Second)},
		LatestPlans: map[int64][]byte{1: indexPlan("orders_status_idx"), 2: seqPlan}}
}

func targetReason(m Mechanism, id int64) string {
	for _, tm := range m.Targets {
		if tm.QueryID == id {
			return tm.Reason
		}
	}
	return "<missing>"
}

// Happy path: target 1 changed plan at the step and names the index, so
// it counts; target 2 changed plan but scans the table, so it is
// unexplained and excluded.
func TestMechanismCountsATargetWhosePlanUsesTheIndex(t *testing.T) {
	m, err := MechanismCheck(mechInput(MechanismIndexCreate))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.Counted, []int64{1}) || !slices.Equal(m.Unexplained, []int64{2}) {
		t.Fatalf("counted %v unexplained %v, want [1] and [2]", m.Counted, m.Unexplained)
	}
	if !m.Passed() || m.Outcome != "" || m.Reason != "" {
		t.Fatalf("gate stopped a step with a mechanism: %+v", m)
	}
	if m.PlanBasis != PlanBasisExecuted {
		t.Fatalf("plan basis %q, want %q (the plan has actual rows)", m.PlanBasis,
			PlanBasisExecuted)
	}
	if got := targetReason(m, 2); got != ReasonMechIndexUnused {
		t.Fatalf("target 2 reason %q, want %q", got, ReasonMechIndexUnused)
	}
	if got := targetReason(m, 1); got != ReasonMechCounted {
		t.Fatalf("target 1 reason %q, want %q", got, ReasonMechCounted)
	}
}

// index_replace follows the same rule as index_create.
func TestMechanismReplaceFollowsTheCreateRule(t *testing.T) {
	m, err := MechanismCheck(mechInput(MechanismIndexReplace))
	if err != nil || !slices.Equal(m.Counted, []int64{1}) {
		t.Fatalf("replace: %+v, %v", m, err)
	}
}

// No target with a mechanism: insufficient_evidence with reason
// no_mechanism, every target unexplained.
func TestMechanismWithoutAnyTargetIsNoMechanism(t *testing.T) {
	in := mechInput(MechanismIndexCreate)
	in.LatestPlans[1] = seqPlan
	m, err := MechanismCheck(in)
	if err != nil {
		t.Fatal(err)
	}
	if m.Passed() || m.Outcome != OutcomeInsufficient || m.Reason != ReasonNoMechanism {
		t.Fatalf("outcome %q reason %q, want %q %q", m.Outcome, m.Reason,
			OutcomeInsufficient, ReasonNoMechanism)
	}
	if len(m.Counted) != 0 || !slices.Equal(m.Unexplained, []int64{1, 2}) {
		t.Fatalf("counted %v unexplained %v", m.Counted, m.Unexplained)
	}
	if !strings.Contains(m.Detail, "orders_status_idx") {
		t.Fatalf("detail %q does not name the index", m.Detail)
	}
}

// Boundary: a plan change exactly one cycle after the step counts; later
// (one nanosecond, or two cycles) does not.
func TestMechanismPlanChangeCycleBoundary(t *testing.T) {
	for _, tc := range []struct {
		after  time.Duration
		counts bool
		reason string
	}{{mechCycle, true, ReasonMechCounted}, {mechCycle + time.Nanosecond, false,
		ReasonMechChangedLate}, {2 * mechCycle, false, ReasonMechChangedLate}} {
		in := mechInput(MechanismIndexCreate)
		in.Targets = []int64{1}
		in.PlanHashes[1] = flip(tc.after)
		m, err := MechanismCheck(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := slices.Contains(m.Counted, 1); got != tc.counts {
			t.Errorf("change at +%s: counted=%v, want %v", tc.after, got, tc.counts)
		}
		if got := targetReason(m, 1); got != tc.reason {
			t.Errorf("change at +%s: reason %q, want %q", tc.after, got, tc.reason)
		}
	}
}

// The plan must change: an unchanged fingerprint is no mechanism even when
// the plan names the index (it used the index before the step too).
func TestMechanismUnchangedPlanDoesNotCount(t *testing.T) {
	in := mechInput(MechanismIndexCreate)
	in.Targets = []int64{1}
	in.PlanHashes[1] = []PlanSample{{At: mechStep.Add(-time.Hour), Hash: "A"},
		{At: mechStep.Add(30 * time.Second), Hash: "A"},
		{At: mechStep.Add(time.Hour), Hash: "A"}}
	m, _ := MechanismCheck(in)
	if m.Passed() || targetReason(m, 1) != ReasonMechUnchanged {
		t.Fatalf("unchanged plan: %+v", m)
	}
}

// Nil/empty: no fingerprints, none before the step, no plan, a nil input
// map and no targets each give a typed reason, never a credited target.
func TestMechanismEmptyEvidenceGivesTypedReasons(t *testing.T) {
	in := mechInput(MechanismIndexCreate)
	in.Targets = []int64{1, 2, 3, 4}
	in.PlanHashes = map[int64][]PlanSample{
		2: {{At: mechStep.Add(30 * time.Second), Hash: "B"}},
		3: flip(30 * time.Second),
		4: {{At: mechStep, Hash: ""}, {At: mechStep.Add(30 * time.Second), Hash: ""}}}
	in.LatestPlans = map[int64][]byte{2: indexPlan("orders_status_idx")}
	m, err := MechanismCheck(in)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]string{1: ReasonMechNoHashes, 2: ReasonMechNoPlanBefore,
		3: ReasonMechNoPlan, 4: ReasonMechNoHashes}
	for id, reason := range want {
		if got := targetReason(m, id); got != reason {
			t.Errorf("target %d reason %q, want %q", id, got, reason)
		}
	}
	if m.Reason != ReasonNoMechanism || len(m.Counted) != 0 {
		t.Fatalf("empty evidence: %+v", m)
	}
	in = mechInput(MechanismIndexCreate)
	in.PlanHashes, in.LatestPlans = nil, nil
	if m, err = MechanismCheck(in); err != nil || m.Reason != ReasonNoMechanism {
		t.Fatalf("nil maps: %+v, %v", m, err)
	}
	in.Targets = nil
	m, err = MechanismCheck(in)
	if err != nil || m.Reason != ReasonNoMechanism || !strings.Contains(m.Detail, "no targets") {
		t.Fatalf("no targets: %+v, %v", m, err)
	}
}

// Invalid input: a latest plan that is not a plan is "no plan" with a
// reason, not an unused index.
func TestMechanismNotAPlanIsNoPlan(t *testing.T) {
	for _, doc := range []string{`{"Query Text":"x"}`, `7`, `not json`} {
		in := mechInput(MechanismIndexCreate)
		in.Targets = []int64{1}
		in.LatestPlans[1] = []byte(doc)
		m, err := MechanismCheck(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := targetReason(m, 1); !strings.HasPrefix(got, ReasonMechNotAPlan) {
			t.Errorf("plan %q: reason %q, want prefix %q", doc, got, ReasonMechNotAPlan)
		}
	}
}

// Invalid input: an unknown class, an index step without an index, cycle
// or step time are caller errors.
func TestMechanismRefusesBadInput(t *testing.T) {
	cases := map[string]func(*MechanismInput){
		"unknown class": func(in *MechanismInput) { in.Class = "vacuum" },
		"empty class":   func(in *MechanismInput) { in.Class = "" },
		"no index":      func(in *MechanismInput) { in.Index = " " },
		"no cycle":      func(in *MechanismInput) { in.Cycle = 0 },
		"negative":      func(in *MechanismInput) { in.Cycle = -time.Second },
		"no step":       func(in *MechanismInput) { in.ExecutedAt = time.Time{} },
	}
	for name, mutate := range cases {
		in := mechInput(MechanismIndexCreate)
		mutate(&in)
		if _, err := MechanismCheck(in); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// A schema-qualified or quoted index name matches EXPLAIN's bare name.
func TestMechanismMatchesQualifiedIndexNames(t *testing.T) {
	for index, plan := range map[string]string{
		"public.orders_status_idx": "orders_status_idx",
		`"public"."Orders_Idx"`:    "Orders_Idx",
		"Orders_Status_Idx":        "orders_status_idx",
		`app."a.b"`:                "a.b",
		`"x""y"`:                   `x"y`,
	} {
		in := mechInput(MechanismIndexCreate)
		in.Index, in.Targets = index, []int64{1}
		in.LatestPlans[1] = []byte(`[{"Plan":{"Node Type":"Index Scan","Index Name":` +
			quoteJSON(plan) + `}}]`)
		m, err := MechanismCheck(in)
		if err != nil || !slices.Equal(m.Counted, []int64{1}) {
			t.Errorf("index %s against plan name %s: %+v, %v", index, plan, m, err)
		}
	}
}

func quoteJSON(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// index_drop reverses the rule: a target counts when its plan changed at
// the step and no longer names the index; a plan that still names it is
// stale.
func TestMechanismDropIsReversed(t *testing.T) {
	in := mechInput(MechanismIndexDrop)
	in.LatestPlans = map[int64][]byte{1: seqPlan, 2: indexPlan("orders_status_idx")}
	m, err := MechanismCheck(in)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.Counted, []int64{1}) || targetReason(m, 2) != ReasonMechIndexStillUsed {
		t.Fatalf("drop: counted %v, target 2 %q", m.Counted, targetReason(m, 2))
	}
	in.PlanHashes[1] = []PlanSample{{At: mechStep, Hash: "A"},
		{At: mechStep.Add(time.Hour), Hash: "A"}}
	if m, _ = MechanismCheck(in); m.Reason != ReasonNoMechanism {
		t.Fatalf("drop without a plan change: %+v", m)
	}
}

// On NEON and Lakebase the evidence is the generic EXPLAIN: the gate
// passes and says "estimated plan".
func TestMechanismEstimatedPlanIsLabelled(t *testing.T) {
	in := mechInput(MechanismIndexCreate)
	in.LatestPlans[1] = estimatedIndexPlan("orders_status_idx")
	m, err := MechanismCheck(in)
	if err != nil || !m.Passed() || m.PlanBasis != PlanBasisEstimated {
		t.Fatalf("estimated plan: %+v, %v", m, err)
	}
	if e := m.Evidence(); e["plan_basis"] != PlanBasisEstimated {
		t.Fatalf("evidence %v does not carry the plan basis", e)
	}
}

// Settings: the mechanism is the executor's mapped metric; no metric is
// no_mechanism.
func TestMechanismSettingsNeedAMappedMetric(t *testing.T) {
	for _, class := range []string{MechanismConfigGUC, MechanismAutovacuum,
		MechanismWALBound} {
		in := MechanismInput{Class: class, Targets: []int64{3, 1}, Metric: "temp_spills"}
		m, err := MechanismCheck(in)
		if err != nil || !m.Passed() || !slices.Equal(m.Counted, []int64{1, 3}) {
			t.Errorf("%s with a metric: %+v, %v", class, m, err)
		}
		in.Metric = ""
		m, err = MechanismCheck(in)
		if err != nil || m.Outcome != OutcomeInsufficient || m.Reason != ReasonNoMechanism {
			t.Errorf("%s without a metric: %+v, %v", class, m, err)
		}
	}
}

// Error propagation: a failed evidence read is mechanism_unavailable with
// its cause, insufficient evidence, and never a revert.
func TestMechanismUnavailableCarriesTheCause(t *testing.T) {
	m := MechanismUnavailable(errors.New("read latest plans: relation does not exist"))
	if m.Passed() || m.Outcome != OutcomeInsufficient {
		t.Fatalf("unavailable passed: %+v", m)
	}
	want := ReasonMechanismUnavailable + ": read latest plans: relation does not exist"
	if m.Reason != want {
		t.Fatalf("reason %q, want %q", m.Reason, want)
	}
	v, stop := m.Gate()
	if !stop || v.Revert || v.Retain || v.Outcome != OutcomeInsufficient || v.Reason != want {
		t.Fatalf("gate verdict %+v (stop %v): must stop without a revert", v, stop)
	}
	if e, ok := v.Evidence["mechanism"].(map[string]any); !ok || e["reason"] != want {
		t.Fatalf("gate evidence %v does not carry the cause", v.Evidence)
	}
	if _, stop := (Mechanism{Counted: []int64{1}}).Gate(); stop {
		t.Fatal("a passed mechanism stopped the step")
	}
	if MechanismUnavailable(nil).Reason != ReasonMechanismUnavailable+": unknown cause" {
		t.Fatal("nil cause not named")
	}
}
