package cases

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Unit tests for the sequence forward-fix script (playbooks-03).
// sequence_playbook_db_test.go runs the script on PostgreSQL.
// No concurrent access tests: the script builder is a pure function of its
// finding and reads only constants.

func sequenceScript(t *testing.T, ident string) *ScriptOutput {
	t.Helper()
	got := projectFinding(sequenceFinding(ident))
	assertActionTypes(t, ident, got, "prepare_sequence_capacity_migration")
	script := got.ActionCandidates[0].ScriptOutput
	if script == nil {
		t.Fatalf("%s: no script output", ident)
	}
	return script
}

// CHECK-03: the review-only contract does not change.
func TestSequenceCandidateKeepsReviewOnlyContract(t *testing.T) {
	c := projectFinding(sequenceFinding("public.orders_id_seq")).ActionCandidates[0]
	if c.RiskTier != "high" || c.RollbackClass != "forward_fix_only" ||
		!reflect.DeepEqual(c.OutputModes, []string{"generate_pr_or_script"}) {
		t.Fatalf("contract = tier %q rollback %q modes %v", c.RiskTier, c.RollbackClass,
			c.OutputModes)
	}
	if c.ProposedSQL != "" || c.BlockedReason == "" {
		t.Fatalf("must stay review-only: sql=%q blocked=%q", c.ProposedSQL, c.BlockedReason)
	}
	labels := c.ScriptOutput.RiskLabels
	if !reflect.DeepEqual(labels, []string{"high", "forward_fix_only", "sequence_exhaustion"}) {
		t.Fatalf("RiskLabels = %v", labels)
	}
}

// Happy path: the script reads the runway from pg_sequences with the
// owning column's type, then carries the guarded ALTER SEQUENCE.
func TestSequenceScriptHasRunwayThenGuardedAlter(t *testing.T) {
	script := sequenceScript(t, "public.orders_id_seq")
	sql := script.MigrationSQL
	runway := strings.Index(sql, "FROM pg_catalog.pg_sequences s")
	alter := strings.Index(sql, `ALTER SEQUENCE "public"."orders_id_seq" AS bigint;`)
	if runway < 0 || alter < 0 || runway > alter {
		t.Fatalf("want runway query before ALTER SEQUENCE (runway=%d alter=%d):\n%s",
			runway, alter, sql)
	}
	for _, col := range []string{"s.data_type", "s.last_value", "s.max_value",
		"s.min_value", "s.increment_by", "s.cycle", "pg_catalog.format_type(",
		"AS binding_limit", "AS remaining_values", "d.deptype IN ('a', 'i')",
		"'pg_catalog.pg_attrdef'::pg_catalog.regclass"} {
		if !strings.Contains(sql[:alter], col) {
			t.Errorf("runway query lacks %q", col)
		}
	}
	check := strings.LastIndex(sql[:alter], "IF narrow IS NOT NULL")
	if check < 0 || !strings.Contains(sql[check:alter], "RAISE EXCEPTION") {
		t.Fatalf("ALTER SEQUENCE is not behind the narrow-column guard:\n%s", sql)
	}
	if !strings.Contains(sql, "set_config('lock_timeout', '5s', true)") {
		t.Fatalf("ALTER SEQUENCE does not run under lock_timeout:\n%s", sql)
	}
}

// Happy path: a bigint sequence owning an integer column needs the column
// widened; the script carries the online widening steps, not only the
// ALTER SEQUENCE.
func TestBigintSequenceScriptCarriesColumnWideningSteps(t *testing.T) {
	f := sequenceFinding("public.orders_id_seq")
	f.Detail["data_type"] = "bigint"
	sql := projectFinding(f).ActionCandidates[0].ScriptOutput.MigrationSQL
	steps := []string{"ADD COLUMN", "trigger", "batches", "NOT VALID",
		"VALIDATE CONSTRAINT", "lock_timeout", "ACCESS EXCLUSIVE", "rewrites"}
	last := -1
	for _, step := range steps[:5] {
		i := strings.Index(sql, step)
		if i <= last {
			t.Fatalf("widening step %q missing or out of order:\n%s", step, sql)
		}
		last = i
	}
	for _, word := range steps[5:] {
		if !strings.Contains(sql, word) {
			t.Fatalf("widening steps lack %q:\n%s", word, sql)
		}
	}
	for _, line := range strings.Split(sql, "\n") {
		if strings.Contains(line, "ADD COLUMN") && !strings.HasPrefix(line, "--") {
			t.Fatalf("column widening must stay commented out: %q", line)
		}
	}
}

// Invalid input: a mixed-case name with an embedded quote stays one
// quoted identifier in the DDL and in every regclass literal.
func TestSequenceScriptQuotesMixedCaseName(t *testing.T) {
	script := sequenceScript(t, `Odd.Seq"x`)
	all := script.MigrationSQL + "\n" + strings.Join(script.VerificationSQL, "\n")
	if !strings.Contains(script.MigrationSQL, `ALTER SEQUENCE "Odd"."Seq""x" AS bigint;`) {
		t.Fatalf("DDL does not quote the name:\n%s", script.MigrationSQL)
	}
	if !strings.Contains(all, `c.oid = '"Odd"."Seq""x"'::regclass`) {
		t.Fatalf("runway does not resolve the quoted name:\n%s", all)
	}
	if strings.Contains(all, "Odd.Seq") {
		t.Fatalf("unquoted name leaked into SQL:\n%s", all)
	}
}

// Invalid input: no identifier text appears on a comment line, so a name
// containing a newline cannot turn the rest of a comment into SQL.
func TestSequenceScriptKeepsNameOutOfComments(t *testing.T) {
	script := sequenceScript(t, "public.evil\nDROP TABLE t")
	for _, line := range strings.Split(script.MigrationSQL, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") && strings.Contains(line, "evil") {
			t.Fatalf("name in comment line %q", line)
		}
	}
}

// Invalid input: a name that contains the guard's dollar-quote tag cannot
// close the DO body early.
func TestSequenceGuardDollarTagAvoidsName(t *testing.T) {
	sql := sequenceScript(t, "public.a$pg_sage$b").MigrationSQL
	open := strings.Index(sql, "DO $")
	if open < 0 {
		t.Fatalf("no DO block:\n%s", sql)
	}
	tag := sql[open+3 : open+3+strings.Index(sql[open+4:], "$")+2]
	if tag == "$pg_sage$" || strings.Count(sql, tag) != 2 {
		t.Fatalf("dollar tag %q collides with the name:\n%s", tag, sql)
	}
}

// Verification reads the catalog view and the column type, never the
// sequence relation's (missing) max_value.
func TestSequenceVerificationReadsPgSequencesAndColumnType(t *testing.T) {
	script := sequenceScript(t, "public.orders_id_seq")
	if len(script.VerificationSQL) != 1 {
		t.Fatalf("VerificationSQL = %v, want the runway query", script.VerificationSQL)
	}
	v := script.VerificationSQL[0]
	for _, want := range []string{"FROM pg_catalog.pg_sequences s", "AS column_type",
		`'"public"."orders_id_seq"'::regclass`} {
		if !strings.Contains(v, want) {
			t.Fatalf("verification lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "ALTER") || strings.Contains(v, "max_value FROM \"") {
		t.Fatalf("verification is not a catalog read:\n%s", v)
	}
}

// Boundary: a descending sequence's runway runs toward min_value and the
// type minimum of its column.
func TestSequenceRunwayHandlesDescendingSequences(t *testing.T) {
	sql := sequenceScript(t, "public.down_seq").MigrationSQL
	for _, want := range []string{"WHEN s.increment_by > 0", "s.min_value",
		"-2147483648", "GREATEST("} {
		if !strings.Contains(sql, want) {
			t.Fatalf("runway lacks descending branch %q:\n%s", want, sql)
		}
	}
}

// Nil/empty: with no sequence named, the case still shows the migration
// as blocked, with the reason and without a script.
func TestSequenceFindingWithoutIdentityIsBlocked(t *testing.T) {
	for _, ident := range []string{"", "   ", "\t"} {
		got := projectFinding(sequenceFinding(ident))
		assertActionTypes(t, "blank "+ident, got, "prepare_sequence_capacity_migration")
		c := got.ActionCandidates[0]
		if c.BlockedReason != "no sequence identity in evidence" || c.ScriptOutput != nil {
			t.Fatalf("blank %q: blocked=%q script=%v", ident, c.BlockedReason, c.ScriptOutput)
		}
		if c.RiskTier != "high" || c.RollbackClass != "forward_fix_only" ||
			!reflect.DeepEqual(c.OutputModes, []string{"generate_pr_or_script"}) ||
			c.ProposedSQL != "" {
			t.Fatalf("blank %q: contract changed: %+v", ident, c)
		}
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(raw), "affected_sequence") {
			t.Fatalf("placeholder name in case: %s", raw)
		}
	}
}
