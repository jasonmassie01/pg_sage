package selfmonitor

import (
	"strings"
	"testing"
)

// No concurrent access tests: both helpers are pure functions.

// Happy path and boundary: a positive decision id round-trips, the largest
// int64 included; 0 and negatives name no decision.
func TestActionApplicationNameRoundTrip(t *testing.T) {
	for _, id := range []int64{1, 77, 1<<62 + 5, 9223372036854775807} {
		name := ActionApplicationName(id)
		if !strings.HasPrefix(name, "pg_sage action:") {
			t.Fatalf("ActionApplicationName(%d) = %q", id, name)
		}
		got, ok := ParseActionApplicationName(name)
		if !ok || got != id {
			t.Fatalf("Parse(%q) = %d, %v; want %d", name, got, ok, id)
		}
		if len(name) > 63 {
			t.Fatalf("%q is longer than application_name keeps (63 bytes)", name)
		}
	}
	if got := ActionApplicationName(77); got != "pg_sage action:77" {
		t.Fatalf("ActionApplicationName(77) = %q", got)
	}
	for _, id := range []int64{0, -1, -9223372036854775808} {
		if got := ActionApplicationName(id); got != ApplicationName {
			t.Fatalf("ActionApplicationName(%d) = %q, want the generic %q", id, got,
				ApplicationName)
		}
	}
}

// Invalid input: only the exact tag parses.
func TestParseActionApplicationNameRejects(t *testing.T) {
	for _, name := range []string{"", "pg_sage", "pg_sage action:", "pg_sage action:0",
		"pg_sage action:-4", "pg_sage action:07", "pg_sage action:12x",
		" pg_sage action:12", "pg_sage action:12 ", "PG_SAGE action:12",
		"pg_sage  action:12", "pg_sage action:9223372036854775808",
		"pg_sage action:12345678901234567890", "pg_sage agent:ci-bot:12",
		"x pg_sage action:12"} {
		if id, ok := ParseActionApplicationName(name); ok {
			t.Fatalf("Parse(%q) = %d, true; want rejected", name, id)
		}
	}
}

// The tag stays one of pg_sage's own names: every collector exclusion
// (ActivityExclusionSQL's ILIKE '%pg_sage%' and its Go twin) still skips it.
func TestActionApplicationNameIsSelf(t *testing.T) {
	name := ActionApplicationName(42)
	if !IsApplicationName(name) {
		t.Fatalf("IsApplicationName(%q) = false; collectors would see action sessions",
			name)
	}
}
