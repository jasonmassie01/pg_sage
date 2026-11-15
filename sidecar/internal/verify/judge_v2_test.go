package verify

import (
	"encoding/json"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

// lab-01c: JudgeV2 maps the attribution (lab-01a) and the guards (lab-01b)
// to a verdict. A gain needs its CI below 0 beyond min_gain_pct and the
// mechanism; a CI inside the bars is "no change" (neutral, kept); a CI
// above 0 beyond regress_pct, or the time guardrail, is a regression; a
// contaminated window, a missing mechanism or too wide a CI is
// insufficient evidence; an up-front unverifiable step is unverifiable.

func v2State() WatchState {
	return WatchState{ID: "w", ActionID: 9, ExecutedAt: fxStep, Window: 2 * time.Hour,
		IndexName: "public.t_idx", Criterion: Criterion{Kind: "per_query_latency",
			TargetIDs: []int64{1, 2}, MinGainPct: 20, RegressPct: 15,
			Window: 2 * time.Hour, HardMax: 72 * time.Hour}}
}

var firstLook = Look{Index: 1, Fraction: 0.5, Alpha: 0.02, Spent: 0.02}

func passed(ids ...int64) Mechanism { return Mechanism{Counted: ids} }

func measured(effectPct, lowPct, highPct float64) Attribution {
	l := func(p float64) float64 { return math.Log1p(p / 100) }
	return Attribution{Measured: true, EffectLog: l(effectPct), CILow: l(lowPct),
		CIHigh: l(highPct), EffectPct: effectPct, Method: MethodDiD, Basis: BasisPreWindow,
		Controls: 3, Guardrail: Guardrail{Measured: true}}
}

func judge(t *testing.T, a Attribution, m Mechanism, c Contamination, look Look) Verdict {
	t.Helper()
	v := JudgeV2(v2State(), V2Evidence{Attribution: a, Mechanism: m, Contamination: c,
		Look: look})
	if _, err := json.Marshal(v.Evidence); err != nil {
		t.Fatalf("evidence is not JSON-safe: %v", err)
	}
	return v
}

func fixtureV2(t *testing.T, in AttributionInput, m Mechanism) Verdict {
	t.Helper()
	in.Alpha = DefaultAlpha
	a := mustAttribute(t, in)
	c, err := DetectContamination(in, fxStep, fxWidth)
	if err != nil {
		t.Fatalf("DetectContamination: %v", err)
	}
	// A look at the full alpha that is not the last, so an insufficient
	// verdict extends instead of reverting.
	return judge(t, a, m, c, Look{Index: 1, Fraction: 1, Alpha: DefaultAlpha,
		Spent: DefaultAlpha})
}

// CHECK-08: a traffic shift with no real change is never improved in v2;
// v1 calls it improved (pinned).
func TestV2_TrafficShiftNeverImproved(t *testing.T) {
	in := fxInput([]int64{1, 2}, []int64{11, 12, 13}, trafficShift())
	before := Pool([]Measurement{Summarize(toIntervals(in.Pre[1])),
		Summarize(toIntervals(in.Pre[2]))})
	after := Pool([]Measurement{Summarize(toIntervals(in.Post[1])),
		Summarize(toIntervals(in.Post[2]))})
	if v1 := Compare(before, after, Thresholds{}); v1.Verdict != OutcomeImproved {
		t.Fatalf("v1 pin: the mix shift must read as improved in v1, got %s", v1.Verdict)
	}
	v := fixtureV2(t, in, passed(1, 2))
	if v.Outcome == OutcomeImproved {
		t.Fatalf("v2 called a traffic shift improved: %+v", v)
	}
	if v.Outcome != OutcomeNeutral && v.Outcome != OutcomeInsufficient {
		t.Fatalf("v2 outcome %q, want neutral or insufficient_evidence", v.Outcome)
	}
	if v.Revert {
		t.Fatalf("v2 reverted a step that changed nothing: %+v", v)
	}
}

// CHECK-08: a regression injected on one control query alone does not
// revert the step in v2 (neutral, kept). v1 does not see the control
// either: its targets are flat, so it reverts the index as no_gain
// (pinned), which is the behaviour v2 changes.
func TestV2_ControlRegressionDoesNotRevert(t *testing.T) {
	stmts := append(flatControls(), stmt{id: 1, preCalls: 50, postCalls: 50, blocks: 100,
		ms: 4, noise: 0.02})
	stmts[2].blockFactor, stmts[2].msFactor = 2, 2 // control 13 only
	in := fxInput([]int64{1}, []int64{11, 12, 13}, stmts)
	ratio := blocksPerCallOf(in.Post[13]) / blocksPerCallOf(in.Pre[13])
	if ratio < 1.9 {
		t.Fatalf("fixture: control 13 must regress (blocks per call x%.2f)", ratio)
	}
	v := fixtureV2(t, in, passed(1))
	if v.Outcome != OutcomeNeutral || !v.Retain || v.Revert {
		t.Fatalf("v2 on a control-only regression: %+v, want neutral and kept", v)
	}
	if ev := v.Evidence["v2"].(map[string]any); ev["controls"] != 3 {
		t.Fatalf("v2 evidence controls %v, want 3", ev["controls"])
	}
	e := &Engine{options: DefaultOptions()}
	state := v2State()
	state.Criterion.TargetIDs = []int64{1}
	v1 := e.decide(state, observations{indexValid: true,
		before: map[int64]Measurement{1: Summarize(toIntervals(in.Pre[1]))},
		after:  map[int64]Measurement{1: Summarize(toIntervals(in.Post[1]))}})
	if v1.Outcome != OutcomeNeutral || !v1.Revert || v1.Reason != "no_gain" {
		t.Fatalf("v1 pin: %+v, want neutral reverted as no_gain", v1)
	}
}

func blocksPerCallOf(bs []Bucket) float64 {
	var blocks, calls float64
	for _, b := range bs {
		blocks += b.Blocks
		calls += float64(b.Calls)
	}
	return blocks / calls
}

func TestV2_ImprovedNeedsCIBelowZeroAndTheMechanism(t *testing.T) {
	v := judge(t, measured(-40, -45, -35), passed(1, 2), Contamination{}, firstLook)
	if v.Outcome != OutcomeImproved || !v.Retain || v.Revert || v.Status != "success" {
		t.Fatalf("a real gain: %+v, want improved and retained", v)
	}
	if v.ObservedPct == nil || *v.ObservedPct != -40 {
		t.Fatalf("observed %v, want -40", v.ObservedPct)
	}
	noMech := Mechanism{Outcome: OutcomeInsufficient, Reason: ReasonNoMechanism,
		Detail: "no target's plan changed"}
	v = judge(t, measured(-40, -45, -35), noMech, Contamination{}, firstLook)
	if v.Outcome != OutcomeInsufficient || v.Retain || v.Revert ||
		!strings.Contains(v.Reason, ReasonNoMechanism) {
		t.Fatalf("a gain without its mechanism: %+v, want insufficient no_mechanism", v)
	}
	if v.Status != "extended" || v.Window != 4*time.Hour {
		t.Fatalf("not the last look: %+v, want extended to 4h", v)
	}
	unavailable := MechanismUnavailable(errObservation)
	v = judge(t, measured(-40, -45, -35), unavailable, Contamination{}, firstLook)
	if v.Outcome != OutcomeInsufficient || !strings.Contains(v.Reason,
		ReasonMechanismUnavailable) {
		t.Fatalf("mechanism unreadable: %+v, want insufficient mechanism_unavailable", v)
	}
}

// Boundary: exactly min_gain_pct with its CI below 0 is a gain; a smaller
// effect with its CI inside the bars is no change.
func TestV2_MinGainBoundary(t *testing.T) {
	if v := judge(t, measured(-20, -25, -15), passed(1), Contamination{}, firstLook); v.
		Outcome != OutcomeImproved {
		t.Fatalf("exactly -20%%: %+v, want improved", v)
	}
	v := judge(t, measured(-19.9, -19.95, -1), passed(1), Contamination{}, firstLook)
	if v.Outcome != OutcomeNeutral || !v.Retain || v.Reason != "no_change" {
		t.Fatalf("-19.9%% inside the bars: %+v, want neutral no_change, kept", v)
	}
}

func TestV2_RegressionNeedsNoMechanism(t *testing.T) {
	noMech := Mechanism{Outcome: OutcomeInsufficient, Reason: ReasonNoMechanism}
	v := judge(t, measured(30, 20, 40), noMech, Contamination{}, firstLook)
	if v.Outcome != OutcomeRegressed || !v.Revert || v.Reason != "query_regression" {
		t.Fatalf("blocks regressed: %+v, want regressed query_regression", v)
	}
	// Exactly regress_pct is not a regression (as v1's strict boundary).
	v = judge(t, measured(15, 14.9, 15.1), passed(1), Contamination{}, firstLook)
	if v.Outcome == OutcomeRegressed {
		t.Fatalf("exactly regress_pct: %+v, must not regress", v)
	}
	a := measured(0, -5, 5)
	a.Guardrail = Guardrail{Measured: true, Regressed: true, EffectPct: 40}
	v = judge(t, a, passed(1), Contamination{}, firstLook)
	if v.Outcome != OutcomeRegressed || !v.Revert || v.Reason != "time_guardrail" {
		t.Fatalf("time guardrail: %+v, want regressed time_guardrail", v)
	}
}

func TestV2_ContaminatedWindowIsInsufficient(t *testing.T) {
	c := Contamination{Contaminated: true, Outcome: OutcomeInsufficient,
		Reason: "contaminated_at=2026-11-01T13:00:00Z"}
	v := judge(t, measured(-40, -45, -35), passed(1), c, firstLook)
	if v.Outcome != OutcomeInsufficient || v.Retain || v.Revert ||
		!strings.Contains(v.Reason, "contaminated_at=") {
		t.Fatalf("contaminated: %+v, want insufficient naming the break", v)
	}
	if ev := v.Evidence["v2"].(map[string]any); ev["cusum"] == nil {
		t.Fatal("the CUSUM record is missing from the evidence")
	}
}

func TestV2_TooNoisyAndUnmeasuredAreInsufficient(t *testing.T) {
	v := judge(t, measured(-10, -50, 60), passed(1), Contamination{}, firstLook)
	if v.Outcome != OutcomeInsufficient || !strings.Contains(v.Reason, "too_noisy") {
		t.Fatalf("wide CI: %+v, want insufficient too_noisy", v)
	}
	nan := measured(-40, -45, -35)
	nan.CILow, nan.CIHigh = math.NaN(), math.NaN()
	if v := judge(t, nan, passed(1), Contamination{}, firstLook); v.Outcome !=
		OutcomeInsufficient {
		t.Fatalf("NaN CI: %+v, want insufficient", v)
	}
	v = judge(t, Attribution{Reason: ReasonNoPreBlocks}, passed(1), Contamination{},
		firstLook)
	if v.Outcome != OutcomeInsufficient || !strings.Contains(v.Reason, ReasonNoPreBlocks) {
		t.Fatalf("unmeasured: %+v, want insufficient with the attribution's reason", v)
	}
}

// State transition: insufficient at the last look reverts as unverifiable
// (as v1 does at HardMax); before it, the window doubles.
func TestV2_LastLookInsufficientReverts(t *testing.T) {
	last := Look{Index: 7, Fraction: 1, Alpha: 0.01, Spent: DefaultAlpha, Final: true}
	v := judge(t, measured(-10, -50, 60), passed(1), Contamination{}, last)
	if !v.Revert || v.Status != "unverifiable" || v.Outcome != OutcomeInsufficient {
		t.Fatalf("last look: %+v, want an unverifiable revert", v)
	}
	v = judge(t, measured(-10, -50, 60), passed(1), Contamination{}, firstLook)
	if v.Revert || v.Status != "extended" ||
		!v.NextEvaluationAt.Equal(fxStep.Add(4*time.Hour)) {
		t.Fatalf("first look: %+v, want extended to executed+4h", v)
	}
}

func TestV2_UpFrontUnverifiable(t *testing.T) {
	p := Preflight{Status: PreflightUnverifiable, Reason: "cannot be verified within 72 h: " +
		"the targets run 4 times a week"}
	v := JudgeV2(v2State(), V2Evidence{Attribution: measured(-40, -45, -35),
		Mechanism: passed(1), Look: firstLook, Preflight: &p})
	if v.Outcome != OutcomeUnverifiable || v.Revert || v.Retain ||
		!strings.Contains(v.Reason, "4 times a week") {
		t.Fatalf("up-front unverifiable: %+v, want unverifiable, neither kept nor reverted",
			v)
	}
	unknown := Preflight{Status: PreflightUnknown, Reason: "no hourly history"}
	v = JudgeV2(v2State(), V2Evidence{Attribution: measured(-40, -45, -35),
		Mechanism: passed(1), Look: firstLook, Preflight: &unknown})
	if v.Outcome != OutcomeImproved {
		t.Fatalf("unknown preflight must not change the verdict: %+v", v)
	}
	if ev := v.Evidence["v2"].(map[string]any); ev["preflight"] == nil {
		t.Fatal("the unknown preflight must be written to the evidence")
	}
}

func TestV2_EvidenceRecordsEveryGuard(t *testing.T) {
	v := judge(t, measured(-38, -45.1, -30.2), passed(1), Contamination{}, firstLook)
	ev, ok := v.Evidence["v2"].(map[string]any)
	if !ok {
		t.Fatalf("evidence %v has no v2 record", v.Evidence)
	}
	for _, key := range []string{"method", "basis", "effect_pct", "ci_low_pct",
		"ci_high_pct", "controls", "mechanism", "cusum", "alpha", "outcome", "reason"} {
		if _, ok := ev[key]; !ok {
			t.Fatalf("v2 evidence lacks %q: %v", key, ev)
		}
	}
	if ev["method"] != MethodDiD || ev["outcome"] != OutcomeImproved {
		t.Fatalf("method %v outcome %v", ev["method"], ev["outcome"])
	}
	low := ev["ci_low_pct"].(float64)
	if math.Abs(low-(-45.1)) > 1e-9 {
		t.Fatalf("ci_low_pct %v, want -45.1", low)
	}
	alpha := ev["alpha"].(map[string]any)
	if alpha["look"] != 1 || alpha["alpha"] != 0.02 {
		t.Fatalf("alpha record %v", alpha)
	}
}

// Nil and zero inputs: an empty attribution and mechanism are insufficient,
// never a revert or a retain on a first look.
func TestV2_ZeroInputsAreInsufficient(t *testing.T) {
	v := JudgeV2(WatchState{}, V2Evidence{})
	if v.Outcome != OutcomeInsufficient || v.Retain || v.Revert {
		t.Fatalf("zero input: %+v, want insufficient", v)
	}
}

func TestV2_ConcurrentJudgementsAgree(t *testing.T) {
	in := fxInput([]int64{1, 2}, []int64{11, 12, 13}, trafficShift())
	want := fixtureV2(t, in, passed(1, 2))
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, _ := Attribute(AttributionInput{Kind: in.Kind, Targets: in.Targets,
				Controls: in.Controls, Pre: in.Pre, Post: in.Post, Alpha: DefaultAlpha,
				RegressPct: in.RegressPct})
			got := JudgeV2(v2State(), V2Evidence{Attribution: a, Mechanism: passed(1, 2),
				Look: Look{Index: 7, Fraction: 1, Alpha: DefaultAlpha, Final: true}})
			if got.Outcome != want.Outcome {
				t.Errorf("concurrent verdict %s, want %s", got.Outcome, want.Outcome)
			}
		}()
	}
	wg.Wait()
}
