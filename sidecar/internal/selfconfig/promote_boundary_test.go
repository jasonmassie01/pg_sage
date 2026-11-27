package selfconfig

import (
	"math/rand"
	"reflect"
	"testing"
	"time"
)

// The default boundary (footprint-04 review): every key starts at its
// default, so evidence that sits at the edge of the first step derives the
// default on one pass and default+step on the next. Those returns to the
// default are noise too: they keep a one-step shadow's soak, and the soak
// clears it only when its derivations (their p90) support the default.

func TestJitterAtTheDefaultBoundaryKeepsTheLedgerBounded(t *testing.T) {
	cfg := defaults()
	rng := rand.New(rand.NewSource(20261108))
	for _, key := range []string{"collector.interval_seconds", "safety.query_timeout_ms",
		"sre.runways.sequence_interval_seconds", "sre.detectors.temp_file_mb"} {
		r := rule(t, key)
		def := r.Get(cfg)
		sim := newSimulation(r, cfg)
		sim.run(3*soakPasses, func(int) Evidence {
			raw := def * (1 + 0.1*(2*rng.Float64()-1))
			if key == "sre.detectors.temp_file_mb" {
				return tempEvidence(raw/4, raw/4)
			}
			return costEvidence(t, key, raw)
		})
		if len(sim.events) > 3 || sim.count(EventCleared) != 0 {
			t.Errorf("%s: jitter at the default wrote %d ledger rows: %+v", key,
				len(sim.events), sim.events)
		}
		if a := sim.state.Active; a != nil && *a != def+r.Step {
			t.Errorf("%s: promoted %v, want the default or %v", key, *a, def+r.Step)
		}
	}
}

func TestEvidenceBackAtTheDefaultClearsTheBoundaryShadow(t *testing.T) {
	key := "collector.interval_seconds"
	sim := newSimulation(rule(t, key), defaults())
	sim.run(soakPasses+5, func(i int) Evidence {
		if i == 0 {
			return costEvidence(t, key, 70) // derives 90: one step above
		}
		return costEvidence(t, key, 50) // derives the default, 60
	})
	if kinds(sim.events) != "shadow,cleared" || sim.state.Shadow != nil ||
		sim.state.Active != nil || sim.state.Value != 60 {
		t.Fatalf("a return to the default did not clear once after the soak: %+v %+v",
			sim.state, sim.events)
	}
	if got := sim.events[1]; got.Previous == nil || *got.Previous != 90 {
		t.Fatalf("cleared row %+v, want previous 90", got)
	}
}

func TestBoundaryNoiseKeepsTheSoak(t *testing.T) {
	prev := shadowState(90, t0)
	prev.Derivations = []float64{70}
	in := baseInput(testRule(nil), prev, withRaw(proposal(60), 55), t0.Add(time.Hour))
	in.Sample = f(2)
	res := Step(in)
	n := res.Next
	if len(res.Events) != 0 || n.Shadow == nil || *n.Shadow != 90 ||
		!n.ShadowSince.Equal(t0) || n.Value != 60 || n.Active != nil {
		t.Fatalf("a return to the default restarted the soak: %+v", res)
	}
	if !reflect.DeepEqual(n.Derivations, []float64{70, 55}) ||
		!reflect.DeepEqual(n.Samples, []float64{1, 1, 1, 2}) {
		t.Fatalf("the noisy pass was not recorded: %v %v", n.Derivations, n.Samples)
	}
}

func TestReturnToTheDefaultClearsAFarOrLegacyShadow(t *testing.T) {
	cases := map[string]State{"two steps away": shadowState(120, t0),
		"no derivations (legacy)": shadowState(90, t0)}
	far := cases["two steps away"]
	far.Derivations = []float64{110}
	cases["two steps away"] = far
	for name, prev := range cases {
		res := Step(baseInput(testRule(nil), prev, withRaw(proposal(60), 55),
			t0.Add(time.Hour)))
		if kinds(res.Events) != "cleared" || res.Next.Shadow != nil {
			t.Errorf("%s: shadow not cleared: %+v", name, res)
		}
	}
	// No raw derivation on this pass: nothing would ever clear it, so clear.
	prev := shadowState(90, t0)
	prev.Derivations = []float64{70}
	res := Step(baseInput(testRule(nil), prev, proposal(60), t0.Add(time.Hour)))
	if kinds(res.Events) != "cleared" {
		t.Fatalf("no raw: shadow not cleared: %+v", res)
	}
}

func TestSoakAtTheValueInForceClearsInsteadOfPromoting(t *testing.T) {
	notWorse := Verdict{Outcome: OutcomeNotWorse, Reason: "cost over budget"}
	for _, p90 := range []bool{true, false} {
		r := testRule(&notWorse)
		r.PromoteP90 = p90
		prev := shadowState(90, t0)
		prev.Derivations = []float64{55, 55, 55, 55, 55, 55, 55, 55, 55, 70}
		res := Step(baseInput(r, prev, withRaw(proposal(60), 55), t0.Add(soak)))
		if kinds(res.Events) != "cleared" || res.Next.Active != nil ||
			res.Next.Shadow != nil || res.Next.Value != 60 {
			t.Errorf("p90 %v: a soak whose evidence supports the default: %+v", p90, res)
		}
	}
}
