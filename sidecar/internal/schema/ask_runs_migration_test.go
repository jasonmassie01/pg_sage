package schema

import (
	"strings"
	"testing"
)

// mcp-07b: sage.ask_runs holds Ask Sage's asynchronous runs (state,
// progress, the stored answer). One step, in trancheFourMigrations.
// No concurrent access tests: the step is a constant string.

func TestAskRunsStep_IsTheOnlyStepCreatingTheTable(t *testing.T) {
	var found []string
	for _, step := range migrationSteps() {
		if strings.Contains(step.sql, "sage.ask_runs") {
			found = append(found, step.name)
		}
	}
	if len(found) != 1 || found[0] != "ddlAskRuns" {
		t.Fatalf("steps naming sage.ask_runs = %v, want only ddlAskRuns", found)
	}
	if trancheFourIndex(t, "ddlAskRuns") < 0 {
		t.Fatalf("trancheFourMigrations = %v, want ddlAskRuns", trancheFourMigrations())
	}
}

func TestAskRunsStep_IsIdempotentAndBounded(t *testing.T) {
	for _, want := range []string{
		"SET LOCAL lock_timeout",
		"CREATE TABLE IF NOT EXISTS sage.ask_runs",
		"REFERENCES sage.ask_conversations(id) ON DELETE CASCADE",
		"REFERENCES sage.ask_messages(id) ON DELETE SET NULL",
		"'queued', 'running', 'done', 'cancelled', 'interrupted', 'failed'",
		"jsonb_array_length(progress) <= 64",
		"length(question) BETWEEN 1 AND 8000",
		"length(actor) BETWEEN 1 AND 200",
		"length(error) <= 500",
		"CREATE INDEX IF NOT EXISTS ask_runs_actor_created",
		"CREATE INDEX IF NOT EXISTS ask_runs_active",
		"WHERE state IN ('queued', 'running')",
	} {
		if !strings.Contains(ddlAskRuns, want) {
			t.Errorf("ddlAskRuns lacks %q:\n%s", want, ddlAskRuns)
		}
	}
	if strings.Contains(ddlAskRuns, "DROP ") {
		t.Errorf("ddlAskRuns drops something:\n%s", ddlAskRuns)
	}
}
