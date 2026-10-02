package srebench

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/sre"
)

// SAGE_BENCH_FAMILIES runs a subset of the families (comma-separated
// trigger kinds); empty runs them all. An unknown family is an error,
// never a silently empty bench.
func TestParseFamilies(t *testing.T) {
	all, err := ParseFamilies("")
	if err != nil || all != nil {
		t.Fatalf("empty = %v (%v), want every family", all, err)
	}
	got, err := ParseFamilies(" checkpoint_storm, lwlock_contention ")
	if err != nil || len(got) != 2 || got[0] != sre.TriggerCheckpoint ||
		got[1] != sre.TriggerLWLock {
		t.Fatalf("subset = %v (%v)", got, err)
	}
	for _, bad := range []string{"nope", "checkpoint_storm,", "operator"} {
		if _, err := ParseFamilies(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestFilterScenarios(t *testing.T) {
	ss := Scenarios()
	if got := FilterScenarios(ss, nil); len(got) != len(ss) {
		t.Fatalf("no filter kept %d of %d", len(got), len(ss))
	}
	got := FilterScenarios(ss, []sre.TriggerKind{sre.TriggerTempFiles})
	if len(got) == 0 {
		t.Fatal("no temp-file scenarios")
	}
	for _, sc := range got {
		if sc.Family != sre.TriggerTempFiles {
			t.Fatalf("filter kept %s (%s)", sc.ID, sc.Family)
		}
	}
}
