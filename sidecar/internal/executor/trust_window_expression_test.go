package executor

import "testing"

// policy-04: the gate's window planner reads trust.maintenance_window as an
// expression; unset and the disabled aliases reach it as "" (never opens
// when the window is configured).
//
// No concurrent access tests: trustWindowExpression is a pure function.

func TestTrustWindowExpression(t *testing.T) {
	for input, want := range map[string]string{
		"":                                     "",
		"   ":                                  "",
		"never":                                "",
		"Disabled":                             "",
		"  weekends ":                          "weekends",
		"weekdays 01:00-05:00 America/Chicago": "weekdays 01:00-05:00 America/Chicago",
	} {
		if got := trustWindowExpression(input); got != want {
			t.Fatalf("trustWindowExpression(%q) = %q, want %q", input, got, want)
		}
	}
}
