package lint

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/workload"
)

// Dogfood round 2: JSONB usage is read from workload statements only.
func TestSlowQuerySQLAppliesTheWorkloadRule(t *testing.T) {
	if !strings.Contains(slowQuerySQL, workload.AdviceSQL("query")) {
		t.Fatalf("slowQuerySQL lacks the workload predicate:\n%s", slowQuerySQL)
	}
}
