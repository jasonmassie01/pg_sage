package cases

import (
	"regexp"
	"testing"
)

// vacuum-08: the analyzer's wraparound_freeze finding is diagnosis only,
// so its case proposes the read-only blocker diagnosis, never a VACUUM;
// autovacuum_tuning (the analyzer's category) maps to the tuning
// candidate and keeps the rollback the finding captured (vacuum-07).

// vacuumStatement finds a VACUUM statement: the keyword opening the text or
// following a semicolon.
var vacuumStatement = regexp.MustCompile(`(?i)(^|;)\s*VACUUM\b`)

func TestVacuumAutopilotWraparoundFreezeIsDiagnosis(t *testing.T) {
	candidate, ok := vacuumAutopilotCandidate(SourceFinding{Category: "wraparound_freeze",
		ObjectIdentifier: "public.orders"}, testClassify)
	// The diagnosis is the read-only freeze probe script (playbooks-04),
	// which reads vacuum statistics; what it must not hold is a VACUUM
	// statement.
	if !ok || candidate.ActionType != "diagnose_freeze_blockers" ||
		candidate.ProposedSQL != diagnosticSQL("diagnose_freeze_blockers") ||
		vacuumStatement.MatchString(candidate.ProposedSQL) ||
		candidate.RiskTier != "safe" {
		t.Fatalf("wraparound_freeze candidate = %#v (%v), want the read-only diagnosis",
			candidate, ok)
	}
}

func TestVacuumAutopilotAutovacuumTuningKeepsCapturedRollback(t *testing.T) {
	f := SourceFinding{Category: "autovacuum_tuning", ObjectIdentifier: "public.t",
		RecommendedSQL: `ALTER TABLE "public"."t" SET (autovacuum_vacuum_scale_factor = 0.01)`,
		RollbackSQL:    `ALTER TABLE "public"."t" RESET (autovacuum_vacuum_scale_factor)`}
	candidate, ok := vacuumAutopilotCandidate(f, testClassify)
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
			RollbackSQL:    rollback}, testClassify)
		if candidate.RollbackClass != "unknown" || candidate.ScriptOutput.RollbackSQL == rollback &&
			rollback != "" {
			t.Fatalf("rollback %q trusted: %#v", rollback, candidate)
		}
	}
}

// vacuum-08 review: ALTER TABLE takes several subcommands separated by
// commas, so a rollback that starts like a reloption change but carries
// a second subcommand (or a second statement) is not trusted either.
func TestVacuumAutopilotAutovacuumTuningRejectsExtraSubcommands(t *testing.T) {
	const change = `ALTER TABLE "public"."t" SET (autovacuum_vacuum_scale_factor = 0.01);`
	for _, rollback := range []string{
		`ALTER TABLE "public"."t" SET (autovacuum_vacuum_scale_factor = 0.2), DROP COLUMN id`,
		`ALTER TABLE "public"."t" RESET (autovacuum_vacuum_scale_factor), OWNER TO mallory`,
		`ALTER TABLE "public"."t" RESET (autovacuum_vacuum_scale_factor); DROP TABLE x`,
		`ALTER TABLE "public"."t" RESET (autovacuum_vacuum_scale_factor) RESET (fillfactor)`,
		`ALTER TABLE "public"."t" SET (autovacuum_vacuum_scale_factor = (0.2))`,
		`ALTER TABLE "public"."t" SET (autovacuum_vacuum_scale_factor = '0.2`,
		`ALTER TABLE "public"."t" SET autovacuum_vacuum_scale_factor = 0.2`,
	} {
		candidate, _ := vacuumAutopilotCandidate(SourceFinding{Category: "autovacuum_tuning",
			RecommendedSQL: change, RollbackSQL: rollback}, testClassify)
		if candidate.RollbackClass != "unknown" || candidate.ScriptOutput.RollbackSQL != "" {
			t.Fatalf("rollback %q trusted: class %q, script rollback %q", rollback,
				candidate.RollbackClass, candidate.ScriptOutput.RollbackSQL)
		}
	}
}

// The analyzer's own rollbacks are trusted: a trailing semicolon, a quoted
// stored value (vacuum-07 keeps the DBA's text, parentheses included) and
// a table name that itself spells " SET (".
func TestVacuumAutopilotAutovacuumTuningTrustsQuotedRollbacks(t *testing.T) {
	for change, rollback := range map[string]string{
		`ALTER TABLE "public"."t" SET (autovacuum_vacuum_scale_factor = 0.01);`: `ALTER TABLE ` +
			`"public"."t" SET (autovacuum_vacuum_scale_factor = ' 0.10)');`,
		`ALTER TABLE "public"."a SET (b" SET (autovacuum_vacuum_scale_factor = 0.01);`: `ALTER ` +
			`TABLE "public"."a SET (b" RESET (autovacuum_vacuum_scale_factor);`,
		`ALTER TABLE public.t SET (autovacuum_vacuum_scale_factor = 0.01)`: `ALTER TABLE ` +
			`public.t SET (autovacuum_vacuum_scale_factor = 0.2, fillfactor = 90)`,
	} {
		candidate, _ := vacuumAutopilotCandidate(SourceFinding{Category: "autovacuum_tuning",
			RecommendedSQL: change, RollbackSQL: rollback}, testClassify)
		if candidate.RollbackClass != "reversible" ||
			candidate.ScriptOutput.RollbackSQL != rollback {
			t.Fatalf("rollback %q for %q not trusted: class %q", rollback, change,
				candidate.RollbackClass)
		}
	}
	candidate, _ := vacuumAutopilotCandidate(SourceFinding{Category: "autovacuum_tuning",
		RecommendedSQL: `ALTER TABLE "public"."a SET (b" SET ` +
			`(autovacuum_vacuum_scale_factor = 0.01)`,
		RollbackSQL: `ALTER TABLE "public"."a" RESET (autovacuum_vacuum_scale_factor)`}, testClassify)
	if candidate.RollbackClass != "unknown" {
		t.Fatal("a rollback of a different (prefix-named) table was trusted")
	}
}
