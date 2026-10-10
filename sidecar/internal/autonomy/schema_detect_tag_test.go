package autonomy

import (
	"strings"
	"testing"
)

// The schema guard's structural scan reads every column of every table:
// the performance gate exempts it from the 100 ms statement mean by its
// tag, up to its own ceiling. The tag must lead the statement so
// pg_stat_statements keeps it; the other schema guard scans stay
// untagged and are judged by the plain budget.
func TestStructuralScanCarriesTheGateTag(t *testing.T) {
	const tag = "/* pg_sage schema_guard:structural v1 */"
	if !strings.HasPrefix(structuralPathologySQL, tag) {
		t.Fatalf("structural scan does not lead with %s:\n%s", tag, structuralPathologySQL)
	}
	if strings.Contains(missingFKIndexSQL, "schema_guard:structural") {
		t.Fatalf("missing-FK scan carries the structural scan's tag:\n%s", missingFKIndexSQL)
	}
}
