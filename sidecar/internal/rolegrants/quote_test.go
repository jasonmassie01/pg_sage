package rolegrants

import "testing"

// QuoteRole leaves a plain lower-case name as it is and double-quotes any
// other, doubling embedded quotes, so a GRANT names exactly that role.
func TestQuoteRole(t *testing.T) {
	cases := map[string]string{
		"sage_agent":   "sage_agent",
		"agent2":       "agent2",
		"_agent":       "_agent",
		"SageAgent":    `"SageAgent"`,
		"sage agent":   `"sage agent"`,
		`Sage "Agent"`: `"Sage ""Agent"""`,
		"2agent":       `"2agent"`,
		"agent-x":      `"agent-x"`,
		"":             `""`,
	}
	for in, want := range cases {
		if got := QuoteRole(in); got != want {
			t.Errorf("QuoteRole(%q) = %q, want %q", in, got, want)
		}
	}
}
