package schema

import (
	"strings"
	"testing"
)

// benches-12: sage.sre_game_days.exercised records what a game day
// exercised (provider, whether the clone holds customer objects, scenario
// ids, and each customer program's binding or skip reason). The column is
// nullable: a row written before this step, or a local bench run, has none.
//
// No concurrent access tests: the migration runs inside bootstrap, which
// is serialised by its advisory lock (covered by the bootstrap tests).

func TestGameDayExercisedStepIsRegisteredAndBounded(t *testing.T) {
	var found []string
	for _, step := range migrationSteps() {
		if strings.Contains(step.sql, "exercised") {
			found = append(found, step.name)
		}
	}
	if len(found) != 1 || found[0] != "ddlGameDayExercised" {
		t.Fatalf("steps naming the column = %v, want only ddlGameDayExercised", found)
	}
	for _, want := range []string{"SET LOCAL lock_timeout",
		"ADD COLUMN IF NOT EXISTS exercised jsonb", "jsonb_typeof(exercised) = 'object'",
		"sre_game_days_exercised_object"} {
		if !strings.Contains(ddlGameDayExercised, want) {
			t.Errorf("the step lacks %q:\n%s", want, ddlGameDayExercised)
		}
	}
}

// Integration: bootstrap runs twice cleanly on a fresh server; the column
// is a nullable jsonb with exactly one object check.
func TestGameDayExercisedBootstrapTwice(t *testing.T) {
	pool := freshPool(t, "game_day_exercised")
	ctx := t.Context()
	for run := 1; run <= 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	var typ string
	var notNull bool
	if err := pool.QueryRow(ctx, `SELECT format_type(atttypid, atttypmod), attnotnull
		FROM pg_attribute WHERE attrelid = 'sage.sre_game_days'::regclass
		AND attname = 'exercised' AND NOT attisdropped`).Scan(&typ, &notNull); err != nil {
		t.Fatalf("read the exercised column: %v", err)
	}
	if typ != "jsonb" || notNull {
		t.Fatalf("exercised = %s not null %v, want a nullable jsonb", typ, notNull)
	}
	var checks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
		WHERE conrelid = 'sage.sre_game_days'::regclass
		AND conname = 'sre_game_days_exercised_object'`).Scan(&checks); err != nil {
		t.Fatal(err)
	}
	if checks != 1 {
		t.Fatalf("%d sre_game_days_exercised_object constraints after two runs, want 1", checks)
	}
}
