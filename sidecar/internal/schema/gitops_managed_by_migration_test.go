package schema

import (
	"strings"
	"testing"
)

// cli-02a's step is a T7 step after lab-03b's, bounded and idempotent in its
// text. No concurrent-writer test: Bootstrap holds its advisory lock.
func TestGitopsManagedByStep_IsAT7StepBoundedAndIdempotent(t *testing.T) {
	steps := trancheSevenMigrations()
	at := -1
	for i, s := range steps {
		if s[0] == "ddlGitopsManagedBy" && s[1] == ddlGitopsManagedBy {
			at = i
		}
	}
	if at < 1 || steps[at-1][0] != "ddlRolloutInstanceEffect" {
		t.Fatalf("ddlGitopsManagedBy at %d in %v, want after ddlRolloutInstanceEffect", at, steps)
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
