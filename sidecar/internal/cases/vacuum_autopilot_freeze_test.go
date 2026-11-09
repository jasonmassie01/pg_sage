package cases

import (
	"strings"
	"testing"
)

// vacuum-08: the analyzer's wraparound_freeze finding is diagnosis only,
// so its case proposes the read-only blocker diagnosis, never a VACUUM;
// autovacuum_tuning (the analyzer's category) maps to the tuning
// candidate and keeps the rollback the finding captured (vacuum-07).

func TestVacuumAutopilotWraparoundFreezeIsDiagnosis(t *testing.T) {
	candidate, ok := vacuumAutopilotCandidate(SourceFinding{Category: "wraparound_freeze",
		ObjectIdentifier: "public.orders"})
	if !ok || candidate.ActionType != "diagnose_freeze_blockers" ||
		strings.Contains(strings.ToUpper(candidate.ProposedSQL), "VACUUM") ||
		candidate.RiskTier != "safe" {
		t.Fatalf("wraparound_freeze candidate = %#v (%v), want the read-only diagnosis",
			candidate, ok)
	}
}

func TestVacuumAutopilotAutovacuumTuningKeepsCapturedRollback(t *testing.T) {
	f := SourceFinding{Category: "autovacuum_tuning", ObjectIdentifier: "public.t",
		RecommendedSQL: `ALTER TABLE "public"."t" SET (autovacuum_vacuum_scale_factor = 0.01)`,
		RollbackSQL:    `ALTER TABLE "public"."t" RESET (autovacuum_vacuum_scale_factor)`}
	candidate, ok := vacuumAutopilotCandidate(f)
	if !ok || candidate.ActionType != "set_table_autovacuum" ||
		candidate.ProposedSQL != f.RecommendedSQL {
		t.Fatalf("autovacuum_tuning candidate = %#v (%v)", candidate, ok)
	}
	if candidate.RollbackClass != "reversible" || candidate.ScriptOutput == nil ||
		candidate.ScriptOutput.RollbackSQL != f.RollbackSQL {
		t.Fatalf("captured rollback lost: class %q script %#v", candidate.RollbackClass,
			candidate.ScriptOutput)
	}
}

// A rollback for another table, or one that is not a reloption change, is
// not trusted: recovery stays unknown.
func TestVacuumAutopilotAutovacuumTuningRejectsForeignRollback(t *testing.T) {
	for _, rollback := range []string{
		`ALTER TABLE "public"."other" RESET (autovacuum_vacuum_scale_factor)`,
		`DROP TABLE "public"."t"`,
		"",
	} {
		candidate, _ := vacuumAutopilotCandidate(SourceFinding{Category: "autovacuum_tuning",
			RecommendedSQL: `ALTER TABLE "public"."t" SET (autovacuum_vacuum_scale_factor = 0.01)`,
			RollbackSQL:    rollback})
		if candidate.RollbackClass != "unknown" || candidate.ScriptOutput.RollbackSQL == rollback &&
			rollback != "" {
			t.Fatalf("rollback %q trusted: %#v", rollback, candidate)
		}
	}
}
