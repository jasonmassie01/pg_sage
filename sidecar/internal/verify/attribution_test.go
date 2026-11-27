package verify

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// State transitions: not applicable. Attribute is a pure function; the
// watch states that use it are lab-01c's.

func happyInput() AttributionInput {
	stmts := append(flatControls(), stmt{id: 1, preCalls: 50, postCalls: 50, blocks: 100,
		ms: 4, blockFactor: 0.6, msFactor: 0.6, noise: 0.02})
	return fxInput([]int64{1}, []int64{11, 12, 13}, stmts)
}

func TestAttribution_DropsMalformedBuckets(t *testing.T) {
	clean := mustAttribute(t, happyInput())
	in := happyInput()
	at := fxStep.Add(time.Hour)
	in.Pre[1] = append(in.Pre[1], Bucket{Start: at, Calls: 10, Blocks: 50,
		TotalMs: math.NaN()})
	in.Post[11] = append(in.Post[11], Bucket{Start: at, Calls: 10, Blocks: -5, TotalMs: 1})
	in.Post[1] = append(in.Post[1], Bucket{Start: at, Calls: -3, Blocks: 5, TotalMs: 1},
		Bucket{Start: at, Calls: 5, Blocks: math.Inf(1), TotalMs: 1})
	got := mustAttribute(t, in)
	if got.EffectLog != clean.EffectLog || got.CILow != clean.CILow ||
		got.Guardrail != clean.Guardrail {
		t.Fatalf("malformed buckets changed the result:\nclean %+v\ngot   %+v", clean, got)
	}
}

func TestAttribution_RefusesTargetAsItsOwnControl(t *testing.T) {
	in := happyInput()
	in.Controls = append(in.Controls, 1)
	_, err := Attribute(in)
	if err == nil || !strings.Contains(err.Error(), "query 1") ||
		!strings.Contains(err.Error(), "own control") {
		t.Fatalf("err = %v, want a refusal naming query 1 as its own control", err)
	}
}

func TestAttribution_RefusesUnknownKind(t *testing.T) {
	in := happyInput()
	in.Kind = "table"
	if _, err := Attribute(in); err == nil || !strings.Contains(err.Error(), `"table"`) {
		t.Fatalf("err = %v, want a refusal naming the kind", err)
	}
	if _, err := Attribute(AttributionInput{}); err == nil {
		t.Fatal("a zero input has no kind and must be refused")
	}
}

func TestAttribution_ZeroPreBlocksIsInsufficient(t *testing.T) {
	in := happyInput()
	for i := range in.Pre[1] {
		in.Pre[1][i].Blocks = 0
	}
	a := mustAttribute(t, in)
	if a.Measured || a.EffectPct != 0 ||
		!strings.Contains(a.Reason, "no pre-action blocks to compare against") {
		t.Fatalf("got measured %v effect %.2f reason %q", a.Measured, a.EffectPct, a.Reason)
	}
}

func assertUnmeasured(t *testing.T, in AttributionInput, reason string) {
	t.Helper()
	a := mustAttribute(t, in)
	if a.Measured || a.EffectLog != 0 || a.EffectPct != 0 || a.Guardrail.Measured {
		t.Fatalf("got an effect: %+v", a)
	}
	if !strings.Contains(a.Reason, reason) {
		t.Fatalf("reason %q, want it to say %q", a.Reason, reason)
	}
}

func TestAttribution_EmptyInputsGiveReasons(t *testing.T) {
	assertUnmeasured(t, AttributionInput{Kind: AttributionIndex}, ReasonNoTargets)
	in := happyInput()
	in.Pre = nil
	assertUnmeasured(t, in, ReasonNoComparableTargets)
	in = happyInput()
	in.Post = Series{}
	assertUnmeasured(t, in, ReasonNoComparableTargets)
	in = happyInput()
	in.EpochBreak = true
	assertUnmeasured(t, in, "suspend")
	in = happyInput()
	in.Pre = fxSide([]stmt{{id: 1, preCalls: 50, blocks: 100, ms: 4, noise: 0.02}},
		fxStep.Add(-2*fxWidth), 2, false)
	assertUnmeasured(t, in, "too few buckets")
}

func TestAttribution_NoControlsNoHistory(t *testing.T) {
	in := happyInput()
	in.Controls, in.Seasonal = nil, nil
	a := mustAttribute(t, in)
	if !a.Measured || a.Method != MethodITS || a.Basis != BasisPreWindow ||
		a.SeasonalWeeks != 0 || a.Controls != 0 {
		t.Fatalf("got %+v, want its on the pre-window with no weeks or controls", a)
	}
	if !strings.Contains(a.Reason, ReasonFewControls) {
		t.Fatalf("reason %q must say %s", a.Reason, ReasonFewControls)
	}
	within(t, "EffectPct", a.EffectPct, -40, 2)
}

func TestAttribution_DropsTargetWithoutPostCalls(t *testing.T) {
	stmts := append(happyStmts(), stmt{id: 2, preCalls: 50, blocks: 30, ms: 1, noise: 0.02})
	a := mustAttribute(t, fxInput([]int64{1, 2}, []int64{11, 12, 13}, stmts))
	if !a.Measured || !strings.Contains(a.Reason, "query 2: no post-action calls") {
		t.Fatalf("measured %v reason %q, want query 2 dropped with a reason", a.Measured,
			a.Reason)
	}
	if _, ok := a.Weights[2]; ok || a.Weights[1] != 1 {
		t.Fatalf("weights %v, want query 1 alone at 1", a.Weights)
	}
}

func happyStmts() []stmt {
	return append(flatControls(), stmt{id: 1, preCalls: 50, postCalls: 50, blocks: 100,
		ms: 4, blockFactor: 0.6, msFactor: 0.6, noise: 0.02})
}

func TestAttribution_ControlCountBoundary(t *testing.T) {
	in := happyInput()
	a := mustAttribute(t, in)
	if a.Method != MethodDiD || a.Controls != 3 {
		t.Fatalf("3 controls: method %q controls %d, want did with 3", a.Method, a.Controls)
	}
	in.Controls = []int64{11, 12}
	a = mustAttribute(t, in)
	if a.Method != MethodITS || a.Controls != 0 || !strings.Contains(a.Reason, "controls<3") {
		t.Fatalf("2 controls: method %q controls %d reason %q, want its and controls<3",
			a.Method, a.Controls, a.Reason)
	}
}

func TestAttribution_SeasonalWeeksBoundary(t *testing.T) {
	a := mustAttribute(t, seasonalInput(3))
	if a.Basis != BasisSeasonal || a.SeasonalWeeks != 3 {
		t.Fatalf("3 weeks: basis %q weeks %d, want seasonal", a.Basis, a.SeasonalWeeks)
	}
	a = mustAttribute(t, seasonalInput(2))
	if a.Basis != BasisPreWindow || a.SeasonalWeeks != 2 {
		t.Fatalf("2 weeks: basis %q weeks %d, want pre_window", a.Basis, a.SeasonalWeeks)
	}
}

// guardrailInput: blocks flat, time per call up by rise% with no noise.
func guardrailInput(rise float64) AttributionInput {
	return fxInput([]int64{1}, nil, []stmt{{id: 1, preCalls: 50, postCalls: 50,
		blocks: 100, ms: 4, msFactor: 1 + rise/100}})
}

func TestAttribution_GuardrailBoundary(t *testing.T) {
	a := mustAttribute(t, guardrailInput(DefaultRegressPct))
	if !a.Guardrail.Measured || a.Guardrail.Regressed {
		t.Fatalf("a rise of exactly regress_pct: %+v, want measured, not a regression",
			a.Guardrail)
	}
	within(t, "guardrail EffectPct", a.Guardrail.EffectPct, DefaultRegressPct, 1e-6)
	within(t, "blocks EffectPct", a.EffectPct, 0, 1e-6)
	a = mustAttribute(t, guardrailInput(DefaultRegressPct+0.1))
	if !a.Guardrail.Regressed || !strings.Contains(a.Guardrail.Reason, "time per call") {
		t.Fatalf("regress_pct+0.1: %+v, want a time-per-call regression", a.Guardrail)
	}
}

func TestAttribution_GlobalSettingHasNoControlGroup(t *testing.T) {
	in := seasonalInput(4)
	in.Kind, in.Controls = AttributionGlobal, []int64{11, 12, 13}
	a := mustAttribute(t, in)
	if a.Method != MethodITS || a.Basis != BasisSeasonal || a.Controls != 0 ||
		!strings.Contains(a.Reason, ReasonGlobal) {
		t.Fatalf("global: %+v, want its on the seasonal basis, no controls, %q", a,
			ReasonGlobal)
	}
}

// Attribute keeps no state: 16 goroutines on one input agree.
func TestAttribution_ConcurrentCallsAgree(t *testing.T) {
	in := happyInput()
	want := mustAttribute(t, in)
	got := make([]Attribution, 16)
	errs := make([]error, 16)
	var wg sync.WaitGroup
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = Attribute(in)
		}(i)
	}
	wg.Wait()
	for i := range got {
		if errs[i] != nil || !reflect.DeepEqual(got[i], want) {
			t.Fatalf("goroutine %d: %v %+v, want %+v", i, errs[i], got[i], want)
		}
	}
}

func TestFrozenWeights_ArePreCallShares(t *testing.T) {
	pre := Series{1: {{Calls: 30}, {Calls: 10}}, 2: {{Calls: 60}}, 3: {{Calls: 5}},
		4: {{Calls: -7}}}
	w := FrozenWeights(pre, []int64{1, 2, 4, 9})
	want := map[int64]float64{1: 0.4, 2: 0.6}
	if len(w) != 2 || math.Abs(w[1]-want[1]) > 1e-12 || math.Abs(w[2]-want[2]) > 1e-12 {
		t.Fatalf("weights %v, want %v (no weight for a target without pre calls)", w, want)
	}
	if got := FrozenWeights(nil, []int64{1}); got == nil || len(got) != 0 {
		t.Fatalf("nil pre: %v, want an empty map", got)
	}
}

// The given frozen weights are used as stored, not recomputed.
func TestAttribution_UsesStoredWeights(t *testing.T) {
	in := fxInput([]int64{1, 2}, []int64{11, 12, 13}, trafficShift())
	in.Weights = map[int64]float64{1: 0.25, 2: 0.75}
	a := mustAttribute(t, in)
	if a.Weights[1] != 0.25 || a.Weights[2] != 0.75 {
		t.Fatalf("weights %v, want the stored 0.25/0.75", a.Weights)
	}
	if in.Weights[1] != 0.25 || len(in.Weights) != 2 {
		t.Fatalf("Attribute modified the caller's weights: %v", in.Weights)
	}
}

type fakeScanner struct{ criterion, baseline []byte }

func (f fakeScanner) Scan(dest ...any) error {
	*dest[1].(*[]byte), *dest[2].(*[]byte) = f.criterion, f.baseline
	return nil
}

// The watch's baseline JSON carries the frozen weights; an older row
// without them decodes to none (judged by v1 only).
func TestWatchState_FrozenWeightsRoundTrip(t *testing.T) {
	state := WatchStateFromRequest(WatchRequest{ID: "w1", ExecutedAt: fxStep,
		FrozenWeights: map[int64]float64{7: 0.3, 8: 0.7}})
	criterion, baseline, err := marshalState(state)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(baseline), `"frozen_weights":{"7":0.3,"8":0.7}`) {
		t.Fatalf("baseline %s lacks the frozen weights", baseline)
	}
	got, err := (&PostgresStateStore{}).scanState(fakeScanner{criterion, baseline})
	if err != nil || !reflect.DeepEqual(got.FrozenWeights, state.FrozenWeights) {
		t.Fatalf("decoded %v (%v), want %v", got.FrozenWeights, err, state.FrozenWeights)
	}
	old, _ := json.Marshal(map[string]any{"watch_id": "w0", "table": "public.t"})
	got, err = (&PostgresStateStore{}).scanState(fakeScanner{criterion, old})
	if err != nil || got.FrozenWeights != nil || got.ID != "w0" {
		t.Fatalf("old row: %+v (%v), want no frozen weights", got, err)
	}
}
