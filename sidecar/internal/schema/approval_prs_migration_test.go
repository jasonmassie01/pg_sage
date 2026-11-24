package schema

import (
	"strings"
	"testing"
)

// cli-02b: ddlApprovalPRs follows pr-01b's step in T7, bounded and
// idempotent in its text. No concurrent access test: the step is a
// constant string (the bootstrap lock serializes it; the DB test boots
// twice).
func TestApprovalPRsStep_FollowsSourceFixPROriginBoundedAndIdempotent(t *testing.T) {
	steps := trancheSevenMigrations()
	at := -1
	for i, s := range steps {
		if s[0] == "ddlApprovalPRs" {
			at = i
		}
	}
	if at < 1 || steps[at][1] != ddlApprovalPRs || steps[at-1][0] != "ddlSourceFixPROrigin" {
		t.Fatalf("ddlApprovalPRs at %d of tranche seven, want right after ddlSourceFixPROrigin",
			at)
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
