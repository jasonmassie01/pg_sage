package autoexplain

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/workload"
)

// Dogfood round 2: plans are captured for workload only; an EXPLAIN or a
// maintenance statement must not take a capture slot (LIMIT) or become a
// plan regression.
func TestCandidateSQLAppliesTheWorkloadRule(t *testing.T) {
	if !strings.Contains(candidateSQL, workload.AdviceSQL("s.query")) {
		t.Fatalf("candidateSQL lacks the workload predicate:\n%s", candidateSQL)
	}
}
