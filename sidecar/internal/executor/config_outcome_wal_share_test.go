package executor

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/verify"
)

// No concurrent access tests: judgeRequestedCheckpoints is a pure function.

// Boundary (advisors-05 review): a window share under 20% is improved even
// when it is more than half the share before; exactly 20% is not.
func TestJudgeRequestedCheckpoints_HealthyShare(t *testing.T) {
	before := ckpt(30, 70, 1) // lifetime share 0.30; half is 0.15
	cases := []struct {
		name    string
		after   map[string]float64
		verdict string
		after1  float64
	}{
		{"9 of 50 is under 20%, above half", ckpt(39, 111, 1), verify.OutcomeImproved, 0.18},
		{"2 of 10 is 20%", ckpt(32, 78, 1), verify.OutcomeNeutral, 0.2},
	}
	for _, c := range cases {
		j := judgeRequestedCheckpoints(before, c.after)
		if j.Verdict != c.verdict || j.Before != 0.3 || j.After != c.after1 {
			t.Errorf("%s: %s %.3f -> %.3f (%q), want %s 0.300 -> %.3f", c.name, j.Verdict,
				j.Before, j.After, j.Reason, c.verdict, c.after1)
		}
		if j.ObservedPct == nil || !strings.Contains(j.Reason, "requested share") {
			t.Errorf("%s: observed %v reason %q, want the shares stated", c.name,
				j.ObservedPct, j.Reason)
		}
	}
}
