package schema

import "testing"

// trancheFourIndex is the position of the named step in
// trancheFourMigrations, or -1 when it is absent; a name listed twice fails
// the test. T4 steps land from many branches, so a step's own test pins
// that it is registered (and any order it depends on), never an absolute
// position; the global last-step pin is in
// TestMigrationStatements_AreTheStepsInOrder.
func trancheFourIndex(t *testing.T, name string) int {
	t.Helper()
	at := -1
	for i, step := range trancheFourMigrations() {
		if step[0] != name {
			continue
		}
		if at >= 0 {
			t.Fatalf("trancheFourMigrations lists %s twice (at %d and %d)", name, at, i)
		}
		at = i
	}
	return at
}

func TestTrancheFourIndex(t *testing.T) {
	steps := trancheFourMigrations()
	if len(steps) == 0 {
		t.Fatal("trancheFourMigrations is empty")
	}
	last := steps[len(steps)-1][0]
	if got := trancheFourIndex(t, last); got != len(steps)-1 {
		t.Fatalf("trancheFourIndex(%s) = %d, want %d", last, got, len(steps)-1)
	}
	if got := trancheFourIndex(t, steps[0][0]); got != 0 {
		t.Fatalf("trancheFourIndex(%s) = %d, want 0", steps[0][0], got)
	}
	if got := trancheFourIndex(t, "ddlNoSuchStep"); got != -1 {
		t.Fatalf("trancheFourIndex(absent) = %d, want -1", got)
	}
	if got := trancheFourIndex(t, ""); got != -1 {
		t.Fatalf("trancheFourIndex(empty) = %d, want -1", got)
	}
}
