package signalid

import "testing"

// No concurrent access or integration tests: classifierOnly is a read-only
// package slice.

// rca-07: log_statement_cancelled is classified but never an incident, so
// it is listed apart from All and returned as a copy.
func TestClassifierOnlyIsOutsideAllAndCopied(t *testing.T) {
	got := ClassifierOnly()
	if len(got) != 1 || got[0] != LogStatementCancelled {
		t.Fatalf("ClassifierOnly() = %v, want [log_statement_cancelled]", got)
	}
	for _, info := range All() {
		if info.ID == LogStatementCancelled {
			t.Fatal("log_statement_cancelled is in All(): it would need an incident tree")
		}
	}
	got[0] = "mutated"
	if ClassifierOnly()[0] != LogStatementCancelled {
		t.Fatal("ClassifierOnly returned the package slice, not a copy")
	}
}
