package cases

import (
	"strings"
	"testing"
	"time"
)

// No concurrent access tests: finding projection is a pure function of its
// argument and reads only constant tables.

func sequenceFinding(ident string) SourceFinding {
	return SourceFinding{
		ID: "f-seq", DatabaseName: "prod", Category: "sequence_exhaustion",
		Severity: SeverityCritical, ObjectType: "sequence", ObjectIdentifier: ident,
		Title:          "Sequence " + ident + " is 91.0% consumed (integer)",
		Recommendation: "Sequence " + ident + " is 91.0% consumed. Consider migrating to bigint.",
		Detail:         map[string]any{"usage_pct": 91.0, "data_type": "integer"},
		ObservedAt:     evidenceAnchor,
	}
}

func TestSequenceExhaustionFindingAddsMigrationScript(t *testing.T) {
	got := projectFinding(sequenceFinding("public.orders_id_seq"))

	assertActionTypes(t, "sequence", got, "prepare_sequence_capacity_migration")
	action := got.ActionCandidates[0]
	assertCandidate(t, action, "prepare_sequence_capacity_migration", "high", "")
	if action.RollbackClass != "forward_fix_only" {
		t.Fatalf("RollbackClass = %q, want forward_fix_only", action.RollbackClass)
	}
	if action.BlockedReason == "" || action.ProposedSQL != "" {
		t.Fatalf("sequence migration must be review-only: blocked=%q sql=%q",
			action.BlockedReason, action.ProposedSQL)
	}
	if action.ExpiresAt == nil || !action.ExpiresAt.Equal(evidenceAnchor.Add(24*time.Hour)) {
		t.Fatalf("ExpiresAt = %v, want last observation + 24h", action.ExpiresAt)
	}
	script := action.ScriptOutput
	if script == nil {
		t.Fatal("expected sequence migration script output")
	}
	if !strings.Contains(script.MigrationSQL, `ALTER SEQUENCE "public"."orders_id_seq" AS`) {
		t.Fatalf("migration SQL does not quote the sequence: %s", script.MigrationSQL)
	}
	if script.Filename != "sequence_capacity_public_orders_id_seq.sql" {
		t.Fatalf("Filename = %q", script.Filename)
	}
	if len(script.VerificationSQL) == 0 ||
		!strings.Contains(script.VerificationSQL[0], `"public"."orders_id_seq"`) {
		t.Fatalf("verification SQL = %v", script.VerificationSQL)
	}
	if !strings.Contains(script.PRBody, "f-seq") {
		t.Fatalf("PR body does not name the finding: %q", script.PRBody)
	}
}

// Invalid input: a hostile identifier stays inside quoted identifiers.
func TestSequenceFindingQuotesHostileIdentifier(t *testing.T) {
	got := projectFinding(sequenceFinding(`public.s"; DROP TABLE t; --`))

	script := got.ActionCandidates[0].ScriptOutput
	if strings.Contains(script.MigrationSQL, `FROM public.s"; DROP`) ||
		!strings.Contains(script.MigrationSQL, `"public"."s""; DROP TABLE t; --"`) {
		t.Fatalf("identifier not quoted: %s", script.MigrationSQL)
	}
}

// inactive_slot carries the analyzer's pg_drop_replication_slot SQL; the
// case offers only the read-only WAL diagnostic, never the drop.
func TestInactiveSlotFindingOffersReadOnlyDiagnostic(t *testing.T) {
	got := projectFinding(SourceFinding{
		ID: "f-slot", DatabaseName: "prod", Category: "inactive_slot",
		Severity: SeverityWarning, ObjectType: "replication_slot",
		ObjectIdentifier: "slot:s1",
		RecommendedSQL:   "SELECT pg_drop_replication_slot('s1');",
		ObservedAt:       evidenceAnchor,
	})

	assertActionTypes(t, "inactive slot", got, "diagnose_wal_replication")
	c := got.ActionCandidates[0]
	assertCandidate(t, c, "diagnose_wal_replication", "safe",
		diagnosticSQL("diagnose_wal_replication"))
	for _, candidate := range got.ActionCandidates {
		if strings.Contains(strings.ToLower(candidate.ProposedSQL), "pg_drop_replication_slot") {
			t.Fatalf("%s proposes a slot drop", candidate.ActionType)
		}
	}
	if c.ExpiresAt == nil || !c.ExpiresAt.After(evidenceAnchor) {
		t.Fatalf("diagnostic expiry %v is not anchored after the observation", c.ExpiresAt)
	}
}

func TestReplicationLagFindingOffersWALDiagnostic(t *testing.T) {
	got := projectFinding(SourceFinding{
		ID: "f-lag", DatabaseName: "prod", Category: "replication_lag",
		Severity: SeverityCritical, ObjectIdentifier: "replica:0:10.0.0.2",
		ObservedAt: evidenceAnchor,
	})

	assertActionTypes(t, "replication lag", got, "diagnose_wal_replication")
	assertCandidate(t, got.ActionCandidates[0], "diagnose_wal_replication", "safe",
		diagnosticSQL("diagnose_wal_replication"))
}

// The executor's runaway finding carries a bare-PID cancel; the case shows
// the diagnostic and a blocked cancel (the runaway policy owns the signal).
func TestRunawayQueryFindingOffersDiagnosticAndBlockedCancel(t *testing.T) {
	got := projectFinding(SourceFinding{
		ID: "f-run", DatabaseName: "prod", Category: "runaway_query",
		Severity: SeverityCritical, ObjectType: "process",
		ObjectIdentifier: "pid:222:policy:default",
		RecommendedSQL:   "SELECT pg_cancel_backend(222);",
		Detail:           map[string]any{"pid": 222, "state": "cancelled"},
		ObservedAt:       evidenceAnchor,
	})

	assertActionTypes(t, "runaway", got, "diagnose_runaway_query", "cancel_backend")
	assertCandidate(t, got.ActionCandidates[0], "diagnose_runaway_query", "safe",
		diagnosticSQL("diagnose_runaway_query"))
	assertBlockedSignal(t, got.ActionCandidates[1], runawayPolicyOwnsSignalReason)
	assertNoBarePIDSignal(t, got)
}

// Boundary: categories next to the new branches keep today's behaviour.
func TestNeighbourFindingCategoriesUnchanged(t *testing.T) {
	got := projectFinding(SourceFinding{
		ID: "f-slow", Category: "slow_replication_slot", ObservedAt: evidenceAnchor,
		RecommendedSQL: "SELECT pg_drop_replication_slot('s2');",
	})
	assertActionTypes(t, "slow slot", got)
}

// The review script's queries name only real catalog columns: a sequence
// relation has no max_value, and pg_get_serial_sequence with empty names errors.
// sequence_playbook_db_test.go runs them on PostgreSQL.
func TestSequenceScriptQueriesTheCatalogForLimits(t *testing.T) {
	script := projectFinding(sequenceFinding("public.orders_id_seq")).
		ActionCandidates[0].ScriptOutput
	all := script.MigrationSQL + "\n" + strings.Join(script.VerificationSQL, "\n")
	for _, bad := range []string{"pg_get_serial_sequence('', '')",
		`max_value FROM "public"`, "affected_sequence"} {
		if strings.Contains(all, bad) {
			t.Fatalf("script contains %q:\n%s", bad, all)
		}
	}
	if !strings.Contains(script.MigrationSQL,
		`d.objid = '"public"."orders_id_seq"'::regclass`) {
		t.Fatalf("owning-column query not scoped to the sequence:\n%s", script.MigrationSQL)
	}
}

// Invalid input: a quote in the name cannot leave the regclass literal.
func TestSequenceScriptEscapesQuoteInRegclassLiteral(t *testing.T) {
	script := projectFinding(sequenceFinding(`public.it's_seq`)).
		ActionCandidates[0].ScriptOutput
	if !strings.Contains(script.MigrationSQL, `'"public"."it''s_seq"'::regclass`) {
		t.Fatalf("quote not doubled in regclass literal:\n%s", script.MigrationSQL)
	}
}
