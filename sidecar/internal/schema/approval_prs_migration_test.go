package schema

import (
	"strings"
	"testing"
)

// cli-02b: ddlApprovalPRs is the last T7 step, bounded and idempotent in
// its text. No concurrent access test: the step is a constant string (the
// bootstrap lock serializes it; the DB test boots twice).
func TestApprovalPRsStep_IsTheLastT7StepBoundedAndIdempotent(t *testing.T) {
	steps := trancheSevenMigrations()
	last := steps[len(steps)-1]
	if last[0] != "ddlApprovalPRs" || last[1] != ddlApprovalPRs {
		t.Fatalf("tranche seven ends %v, want ddlApprovalPRs", last[0])
	}
	all := migrationSteps()
	if all[len(all)-1].name != "ddlApprovalPRs" {
		t.Fatalf("migrationSteps ends %s, want ddlApprovalPRs", all[len(all)-1].name)
	}
	for _, want := range []string{"SET LOCAL lock_timeout",
		"CREATE TABLE IF NOT EXISTS sage.approval_prs",
		"CONSTRAINT approval_prs_one_per_change UNIQUE (database, queue_id)",
		"'opening', 'open', 'open_failed', 'approved', 'rejected', 'refused',",
		"CREATE INDEX IF NOT EXISTS idx_approval_prs_live"} {
		if !strings.Contains(ddlApprovalPRs, want) {
			t.Errorf("ddlApprovalPRs lacks %q", want)
		}
	}
}
