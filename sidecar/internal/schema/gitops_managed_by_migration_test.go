package schema

import (
	"strings"
	"testing"
)

// cli-02a's step is the last T7 step, bounded and idempotent in its text.
// No concurrent-writer test: Bootstrap holds its advisory lock.
func TestGitopsManagedByStep_IsTheLastT7StepBoundedAndIdempotent(t *testing.T) {
	steps := trancheSevenMigrations()
	last := steps[len(steps)-1]
	if last[0] != "ddlGitopsManagedBy" || last[1] != ddlGitopsManagedBy {
		t.Fatalf("tranche seven ends %v, want ddlGitopsManagedBy", last[0])
	}
	for _, want := range []string{"SET LOCAL lock_timeout",
		"ALTER TABLE sage.guard_principals\n    ADD COLUMN IF NOT EXISTS managed_by",
		"ALTER TABLE sage.policy_sets\n    ADD COLUMN IF NOT EXISTS managed_by",
		"IF NOT EXISTS (SELECT 1 FROM pg_constraint",
		"CHECK (source IN ('manual', 'idp', 'config'))",
		"'exception_expired', 'set_retired'",
		"requested_by_user = 0 AND requested_by LIKE 'config:%'"} {
		if !strings.Contains(ddlGitopsManagedBy, want) {
			t.Errorf("ddlGitopsManagedBy lacks %q", want)
		}
	}
}
