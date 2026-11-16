package schema

import (
	"context"
	"strings"
	"testing"
)

// ux-04b: ddlOnboardingSetupSteps adds the setup wizard's three first-done
// times to sage.onboarding. No concurrent access test here: the step is a
// constant string run under the bootstrap lock; the once-only race is
// onboarding.TestRecordSetupStepConcurrentWritersKeepOne.

var setupStepColumns = []string{"model_configured_at", "channel_configured_at",
	"autonomy_chosen_at"}

func TestOnboardingSetupStepsStep_IsLastBoundedAndIdempotent(t *testing.T) {
	steps := trancheFiveMigrations()
	last := steps[len(steps)-1]
	if last[0] != "ddlOnboardingSetupSteps" || last[1] != ddlOnboardingSetupSteps {
		t.Fatalf("last T5 step = %s, want ddlOnboardingSetupSteps", last[0])
	}
	if steps[len(steps)-2][0] != "ddlUserInboxSeenAt" {
		t.Fatalf("ddlOnboardingSetupSteps follows %s, want ddlUserInboxSeenAt",
			steps[len(steps)-2][0])
	}
	if !strings.Contains(ddlOnboardingSetupSteps, "SET LOCAL lock_timeout") ||
		!strings.Contains(ddlOnboardingSetupSteps, "ALTER TABLE sage.onboarding") {
		t.Fatal("the step must alter sage.onboarding under a lock_timeout")
	}
	for _, col := range setupStepColumns {
		if !strings.Contains(ddlOnboardingSetupSteps,
			"ADD COLUMN IF NOT EXISTS "+col+" timestamptz") {
			t.Errorf("the step lacks an idempotent nullable %s", col)
		}
	}
	upper := strings.ToUpper(ddlOnboardingSetupSteps)
	if strings.Contains(upper, "DEFAULT") || strings.Contains(upper, "NOT NULL") ||
		strings.Contains(upper, "DROP") {
		t.Error("the columns must be nullable with no default (NULL is not done yet)")
	}
	for _, stmt := range classifyStep(ddlOnboardingSetupSteps) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}

// Integration: two bootstraps leave three nullable timestamptz columns,
// and a row written before the step keeps NULL in each.
func TestOnboardingSetupStepsMigration_BootstrapTwice(t *testing.T) {
	pool := freshSchemaPool(t, "setupsteps")
	ctx := context.Background()
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap run 1: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.onboarding (database_name, install_kind)
		VALUES ('app', 'new')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap run 2: %v", err)
	}
	for _, col := range setupStepColumns {
		var typ string
		var notNull bool
		if err := pool.QueryRow(ctx, `SELECT format_type(atttypid, atttypmod), attnotnull
			FROM pg_attribute WHERE attrelid = 'sage.onboarding'::regclass
			AND attname = $1 AND NOT attisdropped`, col).Scan(&typ, &notNull); err != nil {
			t.Fatalf("read %s: %v", col, err)
		}
		if typ != "timestamp with time zone" || notNull {
			t.Fatalf("%s is %s (not null %v), want nullable timestamptz", col, typ, notNull)
		}
	}
	var unset bool
	if err := pool.QueryRow(ctx, `SELECT model_configured_at IS NULL
		AND channel_configured_at IS NULL AND autonomy_chosen_at IS NULL
		FROM sage.onboarding WHERE database_name = 'app'`).Scan(&unset); err != nil || !unset {
		t.Fatalf("existing row: unset %v err %v, want all three NULL", unset, err)
	}
}
