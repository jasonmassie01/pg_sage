package schema

import (
	"strings"
	"testing"
)

// ux-09a: ddlNotificationRuleClass adds delivery_class to the notification
// rules. No concurrent access test: the step is a constant string (the
// bootstrap lock serializes it; the DB test boots twice).

func TestNotificationRuleClassStep_IsTheFirstT7Step(t *testing.T) {
	steps := trancheSevenMigrations()
	if len(steps) == 0 || steps[0][0] != "ddlNotificationRuleClass" ||
		steps[0][1] != ddlNotificationRuleClass {
		t.Fatalf("tranche seven starts %v, want ddlNotificationRuleClass", steps)
	}
	all := migrationSteps()
	if all[len(all)-len(steps)].name != "ddlNotificationRuleClass" {
		t.Fatal("tranche seven is not appended at the end of migrationSteps")
	}
}

func TestNotificationRuleClassStep_IsIdempotentAndBounded(t *testing.T) {
	for _, want := range []string{"SET LOCAL lock_timeout",
		"ADD COLUMN IF NOT EXISTS delivery_class text",
		"delivery_class IN ('page', 'ask', 'digest')",
		"CREATE INDEX IF NOT EXISTS idx_notification_rules_class"} {
		if !strings.Contains(ddlNotificationRuleClass, want) {
			t.Errorf("ddlNotificationRuleClass lacks %q", want)
		}
	}
	for _, bad := range []string{"TRUNCATE", "DELETE FROM", "UPDATE sage.", "DROP ",
		"SET NOT NULL", "text NOT NULL", "RENAME"} {
		if strings.Contains(ddlNotificationRuleClass, bad) {
			t.Errorf("ddlNotificationRuleClass runs or names %q", bad)
		}
	}
	for _, stmt := range classifyStep(ddlNotificationRuleClass) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}
