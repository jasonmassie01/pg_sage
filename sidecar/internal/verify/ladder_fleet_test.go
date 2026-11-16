package verify

import (
	"testing"
	"time"
)

// lab-03b: tier 4 of the ladder is a member's measured effect in a
// staggered fleet rollout. At or under the halt bar it passed, above it
// or with a failed verdict it failed, and with no number (flag only) it is
// inconclusive, which never counts as a pass.
func TestFromFleetEffect(t *testing.T) {
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.FixedZone("x", 3600))
	pct := func(v float64) *float64 { return &v }
	for _, c := range []struct {
		name   string
		in     FleetEffect
		result string
		passed bool
	}{
		{"improved", FleetEffect{EffectPct: pct(-30), ControlPct: pct(0)}, LadderPassed, true},
		{"at the bar", FleetEffect{EffectPct: pct(10)}, LadderPassed, true},
		{"above the bar", FleetEffect{EffectPct: pct(10.01)}, LadderFailed, false},
		{"failed verdict", FleetEffect{EffectPct: pct(-50), Failed: true}, LadderFailed, false},
		{"flag only", FleetEffect{}, LadderInconclusive, false},
		{"flag only failed", FleetEffect{Failed: true}, LadderFailed, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := c.in
			in.Database, in.ActionID, in.LimitPct, in.Method = "db1", 7, 10, "v2"
			r := FromFleetEffect(in, at)
			if r.Tier != TierFleet || r.Name != "fleet" || r.Method != MethodLadderFleet ||
				r.Result != c.result || r.Passed() != c.passed || !r.At.Equal(at) ||
				r.At.Location() != time.UTC {
				t.Fatalf("record = %+v", r)
			}
			if r.Evidence["database"] != "db1" || r.Evidence["action_id"] != int64(7) ||
				r.Evidence["limit_pct"] != 10.0 || r.Evidence["method"] != "v2" {
				t.Fatalf("evidence = %+v", r.Evidence)
			}
			if in.EffectPct != nil && (r.Predicted == nil || *r.Predicted != *in.EffectPct ||
				r.Metric != LadderMetricBlocksPerCall) {
				t.Fatalf("predicted = %v %q", r.Predicted, r.Metric)
			}
			if c.result == LadderInconclusive && len(r.Skipped) != 1 {
				t.Fatalf("inconclusive record names no reason: %+v", r)
			}
		})
	}
}
