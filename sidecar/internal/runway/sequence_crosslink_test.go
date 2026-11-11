package runway

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// rules-11: sequence_exhaustion findings name the runway category that
// holds the time-to-limit for the same sequence; the two must not drift.
func TestSequenceExhaustionCrossLinksRunwayCategory(t *testing.T) {
	if analyzer.SequenceRunwayCategory != CategorySequence {
		t.Fatalf("analyzer links %q, runway emits %q",
			analyzer.SequenceRunwayCategory, CategorySequence)
	}
}
