package schema

import (
	"strings"
	"testing"
)

// market-14: the case ticket links are one idempotent T3 step under
// lock_timeout, keyed one ticket per (database, case, system).
//
// No concurrent access tests: the migration runs inside bootstrap, which
// is serialised by its advisory lock (covered by the bootstrap tests).

func TestCaseTicketsStepIsRegisteredAndBounded(t *testing.T) {
	var found []string
	for _, step := range migrationSteps() {
		if strings.Contains(step.sql, "sage.case_tickets (") {
			found = append(found, step.name)
		}
	}
	if len(found) != 1 || found[0] != "ddlCaseTickets" {
		t.Fatalf("steps creating case_tickets = %v, want only ddlCaseTickets", found)
	}
	for _, want := range []string{"SET LOCAL lock_timeout",
		"CREATE TABLE IF NOT EXISTS sage.case_tickets",
		"UNIQUE (database, case_identity_key, system)", "ON DELETE SET NULL",
		"CHECK (system IN ('jira'))"} {
		if !strings.Contains(ddlCaseTickets, want) {
			t.Errorf("the step lacks %q", want)
		}
	}
}

// Integration: bootstrap runs twice cleanly on a fresh server, and the
// unique key refuses a second ticket for one case.
func TestCaseTicketsBootstrapTwice(t *testing.T) {
	pool := freshPool(t, "case_tickets")
	ctx := t.Context()
	for run := 1; run <= 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	insert := `INSERT INTO sage.case_tickets (database, case_identity_key, system)
		VALUES ('db1', 'finding:db1:x', 'jira')`
	if _, err := pool.Exec(ctx, insert); err != nil {
		t.Fatalf("first ticket: %v", err)
	}
	_, err := pool.Exec(ctx, insert)
	if err == nil || !strings.Contains(err.Error(), "unique") {
		t.Fatalf("second ticket err = %v, want a unique violation", err)
	}
}
