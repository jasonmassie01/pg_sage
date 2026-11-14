package lint

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/workload"
)

// Dogfood round 2: JSONB usage is read from workload statements only.
func TestSlowQuerySQLAppliesTheWorkloadRule(t *testing.T) {
	if !strings.Contains(slowQuerySQL(publicStatements), workload.AdviceSQL("query")) {
		t.Fatalf("slowQuerySQL lacks the workload predicate:\n%s", slowQuerySQL(publicStatements))
	}
}

// publicStatements is the pg_stat_statements view the test database
// installs in public; the statements became builders over the qualified
// view in security-01c-q.
const publicStatements = `"public"."pg_stat_statements"`
