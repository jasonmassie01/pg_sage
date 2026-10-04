package selfconfig

import (
	"math"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// The cap at the default holds in both directions, whatever the rule
// derives (added in the post-test audit: every registered rule caps a
// lower bound, so the higher-spends branch had no rule exercising it).

func syntheticRule(widens Widens, derived float64) Rule {
	return Rule{Key: "collector.max_queries", Name: "synthetic", Version: 1,
		Widens: widens, Cap: CapAtDefault, Step: 1, HardMin: 1, HardMax: 100000,
		Derive: func(Evidence, *config.Config) Derivation {
			return Derivation{OK: true, Value: derived, Reason: "synthetic"}
		},
		Get: func(c *config.Config) float64 { return float64(c.Collector.MaxQueries) },
		Set: func(c *config.Config, v float64) { c.Collector.MaxQueries = int(v) },
	}
}

func TestCapAtDefaultWhenHigherSpendsMore(t *testing.T) {
	def := float64(config.DefaultConfig().Collector.MaxQueries)
	for _, derived := range []float64{0, 1, def - 1, def, def + 1, 1e9, math.Inf(1)} {
		p := syntheticRule(WidensWhenHigher, derived).Evaluate(Evidence{}, nil)
		if !p.OK || p.Value > def || p.Bounds.Max != def || p.Bounds.Min != 1 {
			t.Errorf("derived %v: %+v (must stay at or below the default %v)", derived, p, def)
		}
	}
}

func TestCapAtDefaultWhenLowerSpendsMore(t *testing.T) {
	def := float64(config.DefaultConfig().Collector.MaxQueries)
	for _, derived := range []float64{-5, 0, def - 1, def, def + 1, 99999} {
		p := syntheticRule(WidensWhenLower, derived).Evaluate(Evidence{}, nil)
		if !p.OK || p.Value < def || p.Bounds.Min != def || p.Bounds.Max != 100000 {
			t.Errorf("derived %v: %+v (must stay at or above the default %v)", derived, p, def)
		}
	}
}

func TestFixedCapIgnoresTheDefault(t *testing.T) {
	r := syntheticRule(WidensWhenHigher, 5000)
	r.Cap, r.CapNote = CapFixed, "test"
	p := r.Evaluate(Evidence{}, nil)
	if !p.OK || p.Value != 5000 || p.Bounds.Max != 100000 {
		t.Fatalf("fixed cap: %+v", p)
	}
}

func TestEvaluateRejectsANaNDerivation(t *testing.T) {
	p := syntheticRule(WidensWhenHigher, math.NaN()).Evaluate(Evidence{}, nil)
	if p.OK {
		t.Fatalf("a NaN derivation was proposed: %+v", p)
	}
}
