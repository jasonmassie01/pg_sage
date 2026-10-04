package autonomy

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/workload"
)

// Dogfood round 2: the schema guard cites related workload statements; a
// VACUUM or EXPLAIN of the table is not one.
func TestStatementsSQLAppliesTheWorkloadRule(t *testing.T) {
	if !strings.Contains(statementsSQL, workload.AdviceSQL("query")) {
		t.Fatalf("statementsSQL lacks the workload predicate:\n%s", statementsSQL)
	}
}
