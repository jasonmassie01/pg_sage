package signalid

import (
	"regexp"
	"testing"
)

// No concurrent access tests: All returns a fresh copy of a read-only table.
// No integration tests: the package holds constants and touches no database.

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// CHECK-02: 33 ids, 8 snapshot, 22 log, 3 detector.
func TestAllCountsByFamily(t *testing.T) {
	got := map[Family]int{}
	for _, info := range All() {
		got[info.Family]++
	}
	want := map[Family]int{FamilySnapshot: 8, FamilyLog: 22, FamilyDetector: 3}
	if len(All()) != 33 {
		t.Fatalf("All() = %d ids, want 33", len(All()))
	}
	for family, n := range want {
		if got[family] != n {
			t.Errorf("family %s = %d ids, want %d", family, got[family], n)
		}
	}
	if len(got) != len(want) {
		t.Errorf("families = %v, want only %v", got, want)
	}
}

func TestAllIDsAreUniqueWellFormedAndClassified(t *testing.T) {
	seen := map[string]bool{}
	for _, info := range All() {
		if !idPattern.MatchString(info.ID) {
			t.Errorf("id %q is not a lower-case snake_case id", info.ID)
		}
		if seen[info.ID] {
			t.Errorf("id %q listed twice", info.ID)
		}
		seen[info.ID] = true
		if info.Kind == "" {
			t.Errorf("id %q has no kind", info.ID)
		}
	}
}

// Boundary: the family prefix matches the id prefix, so a log id cannot be
// filed under snapshot by mistake.
func TestFamilyMatchesIDPrefix(t *testing.T) {
	prefix := regexp.MustCompile(`^(log|sre)_`)
	for _, info := range All() {
		m := prefix.FindStringSubmatch(info.ID)
		switch {
		case info.Family == FamilyLog && (m == nil || m[1] != "log"):
			t.Errorf("log signal %q lacks the log_ prefix", info.ID)
		case info.Family == FamilyDetector && (m == nil || m[1] != "sre"):
			t.Errorf("detector signal %q lacks the sre_ prefix", info.ID)
		case info.Family == FamilySnapshot && m != nil:
			t.Errorf("snapshot signal %q carries a %s_ prefix", info.ID, m[1])
		}
	}
}

func TestAllReturnsACopy(t *testing.T) {
	first := All()
	first[0].ID = "mutated"
	second := All()
	if second[0].ID != ConnectionsHigh || len(second) != 33 {
		t.Fatalf("All() shares state with callers: %v (%d)", second[0], len(second))
	}
}
