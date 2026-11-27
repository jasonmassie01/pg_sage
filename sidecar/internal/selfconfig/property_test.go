package selfconfig

import (
	"math"
	"math/rand"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// Never-widen property: whatever the evidence (random, extreme, broken)
// and whatever the operator configured around the key, a derived value
// stays inside its rule's bounds, is never NaN, and for every rule capped
// at the product default it never crosses the default in the direction
// that spends more or widens authority.

func randomMeasure(rng *rand.Rand) Measure {
	switch rng.Intn(9) {
	case 0:
		return Measure{}
	case 1:
		return Known(math.NaN())
	case 2:
		return Known(math.Inf(1))
	case 3:
		return Known(-rng.Float64() * 1e6)
	case 4:
		return Known(0)
	case 5:
		return Known(rng.Float64() * 10)
	case 6:
		return Known(rng.Float64() * 1e4)
	case 7:
		return Known(rng.Float64() * 1e9)
	default:
		return Known(math.MaxFloat64 * rng.Float64())
	}
}

func randomEvidence(rng *rand.Rand) Evidence {
	return Evidence{
		Relations: randomMeasure(rng), CatalogScanMs: randomMeasure(rng),
		Sequences: randomMeasure(rng), SequenceScanMs: randomMeasure(rng),
		MaxConnections: randomMeasure(rng), TempBytes: randomMeasure(rng),
		StatsAgeSeconds: randomMeasure(rng), TempBytesPerSecond: randomMeasure(rng),
		CollectorCycleMs: randomMeasure(rng),
	}
}

// randomConfig varies the keys around the derived ones (the cross-key
// bounds read them) within their validated ranges.
func randomConfig(rng *rand.Rand) *config.Config {
	c := config.DefaultConfig()
	r := &c.SRE.Runways
	r.SampleRetentionHours = 1 + rng.Intn(720)
	r.LookbackHours = 1 + rng.Intn(min(168, r.SampleRetentionHours))
	r.MinSamples = 3 + rng.Intn(998)
	c.SRE.Detectors.WindowSeconds = 60 + rng.Intn(3541)
	return c
}

func TestNeverWidenProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(20261004))
	def := config.DefaultConfig()
	for _, r := range Rules() {
		derived := 0
		for i := 0; i < 20000; i++ {
			cfg := randomConfig(rng)
			p := r.Evaluate(randomEvidence(rng), cfg)
			if !p.OK {
				continue
			}
			derived++
			v, b := p.Value, p.Bounds
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("%s: non-finite value %v", r.Key, v)
			}
			if v < b.Min || v > b.Max {
				t.Fatalf("%s: %v outside [%v, %v]", r.Key, v, b.Min, b.Max)
			}
			if v < r.HardMin || v > r.HardMax {
				t.Fatalf("%s: %v outside the hard range [%v, %v]", r.Key, v, r.HardMin,
					r.HardMax)
			}
			if r.Cap != CapAtDefault {
				continue
			}
			d := r.Get(def)
			if r.Widens == WidensWhenHigher && v > d {
				t.Fatalf("%s: derived %v above the default %v (higher spends more)",
					r.Key, v, d)
			}
			if r.Widens == WidensWhenLower && v < d {
				t.Fatalf("%s: derived %v below the default %v (lower spends more)",
					r.Key, v, d)
			}
		}
		if derived < 1000 {
			t.Errorf("%s: only %d of 20000 random cases derived a value; the property "+
				"was barely exercised", r.Key, derived)
		}
	}
}

// The derived value applied to a valid configuration keeps it valid.
func TestDerivedValuesKeepTheConfigurationValid(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 3000; i++ {
		cfg := randomConfig(rng)
		if err := cfg.Validate(); err != nil {
			continue
		}
		for _, r := range Rules() {
			p := r.Evaluate(randomEvidence(rng), cfg)
			if !p.OK {
				continue
			}
			candidate := config.Clone(cfg)
			r.Set(candidate, p.Value)
			if err := candidate.Validate(); err != nil {
				t.Fatalf("%s=%v makes the configuration invalid: %v", r.Key, p.Value, err)
			}
		}
	}
}

// Every rule caps in a declared direction; only fixed-ceiling rules may
// derive past the default, and their ceiling is at most 10x the default.
func TestCapsAreDeclared(t *testing.T) {
	def := config.DefaultConfig()
	for _, r := range Rules() {
		if r.Widens != WidensWhenHigher && r.Widens != WidensWhenLower {
			t.Errorf("%s: widening direction not declared", r.Key)
		}
		if r.Cap == CapFixed {
			if r.HardMax > 10*r.Get(def) {
				t.Errorf("%s: fixed ceiling %v exceeds 10x the default %v", r.Key, r.HardMax,
					r.Get(def))
			}
			if r.CapNote == "" {
				t.Errorf("%s: a fixed ceiling past the default needs a recorded justification",
					r.Key)
			}
		}
	}
}

// Jitter property (footprint-04): evidence jittering by ±10% around a
// step boundary (never more than one step either side: a wider spread is
// a real move) promotes within the soak plus 3 passes, writes at most 3
// ledger rows, and promotes one of the two values the jitter rounds to.
func TestJitterAroundAStepBoundaryPromotesOnce(t *testing.T) {
	rng := rand.New(rand.NewSource(20261108))
	cfg := config.DefaultConfig()
	// lowest boundary at which each rule's comparison favours the move
	// from the default (below it the active value is within budget).
	lowest := map[string]float64{"collector.interval_seconds": 90,
		"safety.query_timeout_ms": 1300, "sre.runways.sequence_interval_seconds": 660}
	keys := []string{"collector.interval_seconds", "safety.query_timeout_ms",
		"sre.runways.sequence_interval_seconds"}
	for i := 0; i < 1000; i++ {
		key := keys[rng.Intn(len(keys))]
		r := rule(t, key)
		b := r.Bounds(cfg)
		lo, hi := lowest[key]/r.Step, math.Floor(b.Max/r.Step)-1
		boundary := (lo + float64(rng.Intn(int(hi-lo)+1))) * r.Step
		amp := math.Min(0.1*boundary, 0.95*r.Step)
		sim := newSimulation(r, cfg)
		sim.run(soakPasses+4, func(int) Evidence {
			return costEvidence(t, key, boundary+(2*rng.Float64()-1)*amp)
		})
		if sim.promotedAt < 0 || sim.promotedAt > soakPasses+3 {
			t.Fatalf("%s around %v (±%v): promoted at pass %d, want by %d; ledger %+v",
				key, boundary, amp, sim.promotedAt, soakPasses+3, sim.events)
		}
		if len(sim.events) > 3 {
			t.Fatalf("%s around %v (±%v): %d ledger rows, want at most 3: %+v", key,
				boundary, amp, len(sim.events), sim.events)
		}
		a := sim.state.Active
		if a == nil || (*a != boundary && *a != boundary+r.Step) {
			t.Fatalf("%s around %v: promoted %v, want %v or %v", key, boundary,
				sim.state.Active, boundary, boundary+r.Step)
		}
	}
}
