//go:build !cgo

package plan

import "testing"

// Without the SQL parser the classifier is degraded: the plan says so and
// marks the forms the regex rules cannot see as not known (fail closed),
// instead of refusing; CHECK-02 runs on the cgo build in CI.
func TestColumnSwapDegradedClassification(t *testing.T) {
	p := mustSwap(t, pkFacts(), intReq())
	if !p.Degraded {
		t.Fatal("plan on a build without the parser is not marked degraded")
	}
	known := map[string]bool{}
	for _, s := range p.Steps {
		known[s.Name] = s.Known
	}
	// The additive allowlist and the neutral statements are proven
	// without the parser; UNIQUE, a rename or a trigger is not.
	for name, want := range map[string]bool{
		"set_lock_timeout": true, "add_column": true, "begin": true, "commit": true,
		"build_unique_index": false, "rename_old_column": false,
		"create_sync_function": false, "backfill": false,
	} {
		if known[name] != want {
			t.Errorf("%s known %v, want %v", name, known[name], want)
		}
	}
}
