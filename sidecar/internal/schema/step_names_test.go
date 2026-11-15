package schema

import "testing"

// No concurrent access tests: MigrationStepNames builds a fresh slice from
// a constant list on every call.

// MigrationStepNames lists every migration step, in order, once each, and
// a caller changing the slice does not change the next answer.
func TestMigrationStepNamesAreTheStepsInOrder(t *testing.T) {
	steps := migrationSteps()
	names := MigrationStepNames()
	if len(names) != len(steps) || len(names) == 0 {
		t.Fatalf("got %d names for %d steps", len(names), len(steps))
	}
	seen := map[string]bool{}
	for i, s := range steps {
		if names[i] != s.name || names[i] == "" {
			t.Fatalf("name %d = %q, want %q", i, names[i], s.name)
		}
		if seen[names[i]] {
			t.Fatalf("step %q listed twice", names[i])
		}
		seen[names[i]] = true
	}
	names[0] = "changed"
	if MigrationStepNames()[0] != steps[0].name {
		t.Fatal("the returned slice aliases the step list")
	}
}
