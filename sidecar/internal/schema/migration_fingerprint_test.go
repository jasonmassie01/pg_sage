package schema

import (
	"context"
	"strings"
	"testing"
)

// config-04b: the pure helpers of the catalog fingerprint. No concurrent
// access tests: these functions are pure.

func TestNamesIdentifier_MatchesWholeIdentifiersOnly(t *testing.T) {
	cases := []struct {
		sql, name string
		want      bool
	}{
		{"ON sage.t (a); idx_a", "idx_a", true},
		{"('idx_a', 'x')", "idx_a", true},
		{"idx_ab", "idx_a", false},
		{"xidx_a idx_a2 idx_a", "idx_a", true},
		{"pre_idx_a", "idx_a", false},
		{"ALTER TABLE sage.config VALIDATE", "sage.config", true},
		{"ALTER TABLE sage.config_audit", "sage.config", false},
		{"", "idx_a", false},
		{"anything", "", false},
	}
	for _, c := range cases {
		if got := namesIdentifier(c.sql, c.name); got != c.want {
			t.Errorf("namesIdentifier(%q, %q) = %v, want %v", c.sql, c.name, got, c.want)
		}
	}
}

func TestValidatesPending(t *testing.T) {
	notValid := []string{"incident_avoided_kind_produced sage.incident_avoided",
		"query_hints_status_check sage.query_hints"}
	cases := []struct {
		name, sql string
		want      bool
	}{
		{"validates by name", "ALTER TABLE sage.incident_avoided VALIDATE CONSTRAINT " +
			"incident_avoided_kind_produced", true},
		{"validates by catalog lookup on the table", "DO $$ BEGIN EXECUTE format(" +
			"'ALTER TABLE sage.query_hints validate constraint %I', c.conname); END $$", true},
		{"names the table but validates nothing", "ALTER TABLE sage.query_hints " +
			"ADD COLUMN IF NOT EXISTS x int", false},
		{"validates another constraint", "ALTER TABLE sage.sre_events VALIDATE " +
			"CONSTRAINT sre_events_event_type_m3", false},
	}
	for _, c := range cases {
		if got := validatesPending(c.sql, notValid); got != c.want {
			t.Errorf("%s: validatesPending = %v, want %v", c.name, got, c.want)
		}
	}
	if validatesPending("VALIDATE CONSTRAINT x", nil) {
		t.Error("no NOT VALID constraint, yet a step validates one")
	}
}

// State transition: a trusted ledger still runs a recorded step while the
// constraint it validates is NOT VALID, and skips it once validated.
func TestMigrationLedger_RunsAStepWhileItsConstraintIsNotValid(t *testing.T) {
	sql := "ALTER TABLE sage.t VALIDATE CONSTRAINT t_check"
	step := migrationStep{name: "ddlV", sql: sql}
	l := loadedLedger(map[string]string{"ddlV": stepChecksum(sql)})
	l.notValid = []string{"t_check sage.t"}
	if skip, err := l.decide(step); err != nil || skip {
		t.Fatalf("NOT VALID: skip=%v err=%v, want the step to run", skip, err)
	}
	l.notValid = nil
	if skip, err := l.decide(step); err != nil || !skip {
		t.Fatalf("validated: skip=%v err=%v, want skip", skip, err)
	}
}

// The fingerprint reads no deparse function: they lock the relations they
// describe, and a boot must not wait behind a table lock to read it.
func TestCatalogFingerprint_UsesNoLockingDeparse(t *testing.T) {
	for _, fn := range []string{"pg_get_expr", "pg_get_viewdef", "pg_get_indexdef",
		"pg_get_constraintdef", "pg_get_triggerdef", "pg_get_ruledef"} {
		if strings.Contains(catalogFingerprintSQL, fn) {
			t.Errorf("catalogFingerprintSQL calls %s", fn)
		}
	}
}

func TestIsDataStatement(t *testing.T) {
	cases := []struct {
		sql  string
		want bool
	}{
		{"UPDATE sage.findings SET x = 1", true},
		{"/* c */ INSERT INTO sage.toil_model VALUES (1)", true},
		{"DELETE FROM sage.config WHERE key LIKE 'retired.%'", true},
		{"WITH r AS (SELECT 1) DELETE FROM sage.t USING r", true},
		{"DO $d$ BEGIN UPDATE sage.findings SET x = 1; EXCEPTION WHEN " +
			"lock_not_available THEN RAISE NOTICE 'later'; END $d$", true},
		{"DO $d$ BEGIN IF NOT EXISTS (SELECT 1) THEN ALTER TABLE sage.t ADD c int; " +
			"UPDATE sage.t SET c = 1; END IF; END $d$", false},
		{"DO $d$ BEGIN EXECUTE 'UPDATE sage.t SET c = 1'; END $d$", false},
		{"DO $d$ BEGIN PERFORM sage.f(); UPDATE sage.t SET c = 1; END $d$", false},
		{"DO $d$ BEGIN RAISE NOTICE 'x'; END $d$", false},
		{"CREATE OR REPLACE FUNCTION sage.f() RETURNS int LANGUAGE sql AS 'UPDATE " +
			"sage.t SET c = 1 RETURNING 1'", false},
		{"UPDATE public.t SET x = 1", false},
		{"ALTER TABLE sage.t ADD COLUMN IF NOT EXISTS c int", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isDataStatement(c.sql); got != c.want {
			t.Errorf("isDataStatement(%q) = %v, want %v", c.sql, got, c.want)
		}
	}
}

// Happy path: a skipped step keeps its settings and data statements, in
// order; a step without data statements keeps nothing.
func TestDataStatements(t *testing.T) {
	step := migrationStep{name: "ddlMixed", sql: "SET LOCAL lock_timeout = '2000ms';" +
		"ALTER TABLE sage.t ADD COLUMN IF NOT EXISTS c int;" +
		"CREATE OR REPLACE FUNCTION sage.f() RETURNS int LANGUAGE sql AS 'SELECT 1';" +
		"UPDATE sage.t SET c = 0 WHERE c IS NULL"}
	want := "SET LOCAL lock_timeout = '2000ms';UPDATE sage.t SET c = 0 WHERE c IS NULL"
	if got := dataStatements(step); got != want {
		t.Fatalf("dataStatements = %q, want %q", got, want)
	}
	if got := dataStatements(migrationStep{sql: testColumnStep}); got != "" {
		t.Fatalf("a DDL-only step keeps %q", got)
	}
}

// State transition: a trusted recorded step runs its data statements
// only, and does not mark the catalog changed.
func TestRunMigrationStep_SkippedStepRunsOnlyItsDataStatements(t *testing.T) {
	sql := testColumnStep + ";UPDATE sage.t SET a = 0 WHERE a IS NULL"
	db := &ledgerExecDB{}
	l := loadedLedger(map[string]string{"ddlX": stepChecksum(sql)})
	ctx := withMigrationLedger(context.Background(), l)
	outcome, err := runMigrationStep(ctx, db, migrationStep{name: "ddlX", sql: sql})
	if err != nil || outcome != stepRerun {
		t.Fatalf("outcome=%v err=%v, want stepRerun", outcome, err)
	}
	if len(db.execs) != 1 || strings.Contains(db.execs[0], "ALTER TABLE") ||
		!strings.Contains(db.execs[0], "UPDATE sage.t") {
		t.Fatalf("execs = %q, want the UPDATE alone", db.execs)
	}
	if l.ran {
		t.Fatal("data statements marked the catalog changed")
	}
}
