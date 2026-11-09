package selfconfig

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Hysteresis of the soak (footprint-04): a candidate that moves one step
// from the shadow, on the same side of the value in force, is noise and
// keeps the soak; two or more steps or a change of side restart it. A
// derived value in force is not re-derived for a one-step move.

func withRaw(p Proposal, raw float64) Proposal {
	p.Raw = &raw
	return p
}

func TestOneStepMoveKeepsTheSoak(t *testing.T) {
	prev := shadowState(120, t0)
	prev.Samples, prev.Derivations = []float64{1, 2, 3}, []float64{118}
	in := baseInput(testRule(nil), prev, withRaw(proposal(150), 140), t0.Add(2*time.Hour))
	in.Sample = f(4)
	res := Step(in)
	n := res.Next
	if n.Shadow == nil || *n.Shadow != 120 || !n.ShadowSince.Equal(t0) ||
		n.Status != StatusShadow || n.Active != nil {
		t.Fatalf("a one-step move restarted or changed the soak: %+v", n)
	}
	if !reflect.DeepEqual(n.Samples, []float64{1, 2, 3, 4}) ||
		!reflect.DeepEqual(n.Derivations, []float64{118, 140}) {
		t.Fatalf("samples %v derivations %v", n.Samples, n.Derivations)
	}
	if len(res.Events) != 0 {
		t.Fatalf("a one-step move wrote ledger rows: %+v", res.Events)
	}
}

func TestOneStepMoveDownKeepsTheSoak(t *testing.T) {
	prev := shadowState(150, t0)
	res := Step(baseInput(testRule(nil), prev, withRaw(proposal(120), 100),
		t0.Add(time.Hour)))
	if res.Next.Shadow == nil || *res.Next.Shadow != 150 ||
		!res.Next.ShadowSince.Equal(t0) || len(res.Events) != 0 {
		t.Fatalf("a one-step move below the shadow restarted the soak: %+v", res)
	}
}

func TestTwoStepMoveRestartsTheSoak(t *testing.T) {
	prev := shadowState(120, t0)
	prev.Samples, prev.Derivations = []float64{1, 2, 3}, []float64{118, 119}
	now := t0.Add(soak + time.Hour)
	res := Step(baseInput(testRule(nil), prev, withRaw(proposal(180), 175), now))
	n := res.Next
	if n.Shadow == nil || *n.Shadow != 180 || !n.ShadowSince.Equal(now) ||
		len(n.Samples) != 0 || n.Active != nil {
		t.Fatalf("soak not restarted on a two-step move: %+v", n)
	}
	if !reflect.DeepEqual(n.Derivations, []float64{175}) {
		t.Fatalf("a new shadow keeps only its own derivation: %v", n.Derivations)
	}
	if kinds(res.Events) != "shadow" || *res.Events[0].Value != 180 ||
		*res.Events[0].Previous != 60 {
		t.Fatalf("events %+v", res.Events)
	}
}

// The value in force is 75 (an operator's configuration value between two
// steps): 90 and 60 are one step apart but on opposite sides of it.
func TestDirectionChangeRestartsTheSoak(t *testing.T) {
	prev := shadowState(90, t0)
	prev.Value = 75
	in := baseInput(testRule(nil), prev, withRaw(proposal(60), 55), t0.Add(time.Hour))
	in.Running, in.Default = 75, 75
	res := Step(in)
	n := res.Next
	if n.Shadow == nil || *n.Shadow != 60 || !n.ShadowSince.Equal(t0.Add(time.Hour)) {
		t.Fatalf("a change of side kept the soak: %+v", n)
	}
	if kinds(res.Events) != "shadow" {
		t.Fatalf("events %+v", res.Events)
	}
}

func derivedState(v float64) State {
	return State{Key: "collector.interval_seconds", Status: StatusDerived, Value: v,
		Active: f(v), RuleVersion: 3}
}

func TestOneStepFromTheDerivedValueStartsNoShadow(t *testing.T) {
	for _, cand := range []float64{150, 210} {
		in := baseInput(testRule(nil), derivedState(180), withRaw(proposal(cand), cand),
			t0)
		in.Running = 180
		res := Step(in)
		if res.Next.Shadow != nil || res.Next.Status != StatusDerived ||
			res.Next.Value != 180 || res.Apply || len(res.Events) != 0 {
			t.Fatalf("candidate %v one step from the derived 180 started a shadow: %+v",
				cand, res)
		}
	}
	in := baseInput(testRule(nil), derivedState(180), withRaw(proposal(120), 118), t0)
	in.Running = 180
	if res := Step(in); res.Next.Shadow == nil || *res.Next.Shadow != 120 ||
		kinds(res.Events) != "shadow" {
		t.Fatalf("a two-step move from the derived value started no shadow: %+v", res)
	}
}

func TestOneStepFromTheDerivedValueClearsAFarShadow(t *testing.T) {
	prev := derivedState(180)
	prev.Status, prev.Shadow, prev.ShadowSince = StatusShadow, f(240), t0
	in := baseInput(testRule(nil), prev, withRaw(proposal(150), 150), t0.Add(time.Hour))
	in.Running = 180
	res := Step(in)
	if res.Next.Shadow != nil || res.Next.Status != StatusDerived ||
		kinds(res.Events) != "cleared" {
		t.Fatalf("result %+v", res)
	}
}

// Without a derived value in force, a one-step derivation from the
// default still enters shadow (the default is not noise).
func TestOneStepFromTheDefaultStillShadows(t *testing.T) {
	res := Step(baseInput(testRule(nil), State{}, withRaw(proposal(90), 85), t0))
	if res.Next.Shadow == nil || *res.Next.Shadow != 90 || kinds(res.Events) != "shadow" ||
		!reflect.DeepEqual(res.Next.Derivations, []float64{85}) {
		t.Fatalf("result %+v", res)
	}
}

func TestDerivationsAreCapped(t *testing.T) {
	prev := shadowState(120, t0)
	for i := 0; i < MaxSoakSamples; i++ {
		prev.Derivations = append(prev.Derivations, float64(i))
	}
	res := Step(baseInput(testRule(nil), prev, withRaw(proposal(120), -7),
		t0.Add(time.Hour)))
	d := res.Next.Derivations
	if len(d) != MaxSoakSamples || d[len(d)-1] != -7 || d[0] != 1 {
		t.Fatalf("derivations len %d first %v last %v", len(d), d[0], d[len(d)-1])
	}
}

func TestPercentile90(t *testing.T) {
	if _, ok := percentile90(nil); ok {
		t.Fatal("p90 of nothing reported a value")
	}
	ten := []float64{10, 1, 9, 2, 8, 3, 7, 4, 6, 5}
	for _, c := range []struct {
		in   []float64
		want float64
	}{{[]float64{5}, 5}, {ten, 9}, {[]float64{1, 2}, 2},
		{[]float64{3, 1, 2, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20},
			18}} {
		before := append([]float64(nil), c.in...)
		got, ok := percentile90(c.in)
		if !ok || got != c.want {
			t.Errorf("p90(%v) = %v %v, want %v", c.in, got, ok, c.want)
		}
		if !reflect.DeepEqual(c.in, before) {
			t.Errorf("p90 reordered its input: %v", c.in)
		}
	}
}

func p90Input(promoteP90 bool, derivations []float64) StepInput {
	notWorse := Verdict{Outcome: OutcomeNotWorse, Reason: "cost over budget"}
	r := testRule(&notWorse)
	r.PromoteP90 = promoteP90
	prev := shadowState(120, t0)
	prev.Derivations = derivations
	return baseInput(r, prev, withRaw(proposal(120), 100), t0.Add(soak))
}

func TestCostRulePromotesTheP90OfTheSoak(t *testing.T) {
	noisy := []float64{91, 92, 93, 94, 95, 96, 97, 98, 145, 150}
	res := Step(p90Input(true, noisy))
	n := res.Next
	if n.Active == nil || *n.Active != 150 || n.Value != 150 || !res.Apply ||
		kinds(res.Events) != "promoted" || *res.Events[0].Value != 150 {
		t.Fatalf("p90 not promoted (want roundUp(145) = 150): %+v", res)
	}
	if n.Derivations != nil || n.Samples != nil {
		t.Fatalf("promotion kept the soak's derivations: %+v", n)
	}
	other := Step(p90Input(false, noisy))
	if other.Next.Active == nil || *other.Next.Active != 120 {
		t.Fatalf("a non-cost rule did not promote its shadow: %+v", other)
	}
	// A state saved before derivations were kept promotes its shadow.
	legacy := Step(p90Input(true, nil))
	if legacy.Next.Active == nil || *legacy.Next.Active != 120 {
		t.Fatalf("legacy state: %+v", legacy)
	}
}

func TestP90PromotionIsClampedToTheBounds(t *testing.T) {
	in := p90Input(true, []float64{91, 92, 93, 94, 95, 96, 97, 98, 145, 150})
	in.Proposal.Bounds.Max = 130
	res := Step(in)
	if res.Next.Active == nil || *res.Next.Active != 130 {
		t.Fatalf("p90 promotion outside the bounds: %+v", res.Next)
	}
}

func TestCostRulesPromoteTheP90(t *testing.T) {
	want := map[string]bool{"collector.interval_seconds": true,
		"safety.query_timeout_ms": true, "sre.runways.sequence_interval_seconds": true}
	for _, r := range Rules() {
		if r.PromoteP90 != want[r.Key] {
			t.Errorf("%s: PromoteP90 %v, want %v", r.Key, r.PromoteP90, want[r.Key])
		}
	}
}

func TestEvaluateKeepsTheRawDerivation(t *testing.T) {
	p := rule(t, "collector.interval_seconds").Evaluate(
		Evidence{CollectorCycleMs: Known(1234)}, defaults())
	if !p.OK || p.Raw == nil || math.Abs(*p.Raw-123.4) > 1e-9 || p.Value != 150 {
		t.Fatalf("proposal %+v", p)
	}
	missing := rule(t, "collector.interval_seconds").Evaluate(Evidence{}, defaults())
	if missing.OK || missing.Raw != nil {
		t.Fatalf("proposal without evidence %+v", missing)
	}
}

func TestHeldOnlyOnAnOutcomeChange(t *testing.T) {
	v := Verdict{Outcome: OutcomeWorse, Reason: "costs 300 ms per run"}
	r := testRule(&v)
	prev := shadowState(120, t0)
	first := Step(baseInput(r, prev, proposal(120), t0.Add(soak)))
	if kinds(first.Events) != "held" {
		t.Fatalf("first hold %+v", first.Events)
	}
	v.Reason = "costs 310 ms per run"
	again := Step(baseInput(r, first.Next, proposal(120), t0.Add(soak+time.Hour)))
	if len(again.Events) != 0 || again.Next.ShadowReason != "costs 310 ms per run" {
		t.Fatalf("a new reason with the same outcome: events %+v reason %q",
			again.Events, again.Next.ShadowReason)
	}
	v = Verdict{Outcome: OutcomeInsufficient, Reason: "2 of 3 soak samples measured"}
	changed := Step(baseInput(r, again.Next, proposal(120), t0.Add(soak+2*time.Hour)))
	if kinds(changed.Events) != "held" ||
		changed.Events[0].Reason != "2 of 3 soak samples measured" {
		t.Fatalf("an outcome change was not recorded: %+v", changed.Events)
	}
}

// 48 hourly passes past the soak with a worsening measure: the reason text
// changes every pass (it embeds the slowest scan), the outcome never does.
func TestDriftingHeldReasonAddsOneRow(t *testing.T) {
	sim := newSimulation(rule(t, "safety.query_timeout_ms"), defaults())
	n := soakPasses + 48
	sim.run(n, func(i int) Evidence {
		return Evidence{CatalogScanMs: Known(130 + 40*float64(i)/float64(n-1))}
	})
	if got := sim.count(EventHeld); got != 1 {
		t.Fatalf("%d held rows over %d passes, want 1: %+v", got, n, sim.events)
	}
	if len(sim.events) != 2 || sim.events[0].Kind != EventShadow {
		t.Fatalf("ledger %+v", sim.events)
	}
	st := sim.state
	if st.Shadow == nil || !st.ShadowSince.Equal(t0) || st.Active != nil ||
		!strings.Contains(st.ShadowReason, "170 ms") {
		t.Fatalf("state %+v", st)
	}
}

// A shadow kept by hysteresis may sit one step past bounds that narrowed
// during its soak: the promoted value is clamped into the bounds.
func TestShadowPromotionIsClampedToTheBounds(t *testing.T) {
	in := p90Input(false, nil)
	in.Proposal.Bounds.Max = 100
	in.Proposal.Value = 100
	res := Step(in)
	if res.Next.Active == nil || *res.Next.Active != 100 || kinds(res.Events) != "promoted" {
		t.Fatalf("shadow promoted outside the bounds: %+v", res)
	}
}
