package rolegrants

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/sqlident"
)

// Role names in a GRANT are quoted by sqlident.Role: a plain lower-case
// name stays as it is, any other is double-quoted with embedded quotes
// doubled, and a key word such as user or all is quoted so the GRANT parses.
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
		"user":         `"user"`,
		"all":          `"all"`,
	}
	for in, want := range cases {
		if got := sqlident.Role(in); got != want {
			t.Errorf("Role(%q) = %q, want %q", in, got, want)
		}
	}
}
