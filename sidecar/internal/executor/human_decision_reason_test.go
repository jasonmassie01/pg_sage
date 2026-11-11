package executor

import "testing"

// policy-07: readers of the ledger render a recorded reason as the
// operator surfaces render a verdict.
//
// No concurrent access tests: HumanDecisionReason is a pure function.

func TestHumanDecisionReason(t *testing.T) {
	cases := []struct{ reason, detail, want string }{
		{"rate_limit_exceeded", "next frees at 18:00",
			"rate limit exceeded (next frees at 18:00)"},
		{"outside_maintenance_window", "", "outside maintenance window"},
		{"budget_exceeded", "   ", "budget exceeded"},
		{"", "", ""},
		{"", "orphan detail", " (orphan detail)"},
	}
	for _, tc := range cases {
		if got := HumanDecisionReason(tc.reason, tc.detail); got != tc.want {
			t.Errorf("HumanDecisionReason(%q, %q) = %q, want %q", tc.reason, tc.detail, got,
				tc.want)
		}
	}
}
