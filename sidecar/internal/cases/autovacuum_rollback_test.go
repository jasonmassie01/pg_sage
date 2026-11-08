package cases

import "testing"

// The executor may capture rollback state when it applies a change. A case
// proposal has not done that yet, and RESET is not the inverse of a prior SET.
func TestAutovacuumTuningDoesNotPromiseUncapturedRecovery(t *testing.T) {
	if got := rollbackClassForAction("set_table_autovacuum"); got != "unknown" {
		t.Fatalf("rollbackClassForAction = %q, want unknown", got)
	}
	candidate := autovacuumTuningCandidate(SourceFinding{
		RecommendedSQL: "ALTER TABLE public.t SET (autovacuum_vacuum_scale_factor = 0.05)",
	})
	if candidate.RollbackClass != "unknown" || candidate.RecoveryDetail == "" || candidate.ScriptOutput.RollbackSQL != "" {
		t.Fatalf("candidate must require captured previous settings: %#v", candidate)
	}
}
