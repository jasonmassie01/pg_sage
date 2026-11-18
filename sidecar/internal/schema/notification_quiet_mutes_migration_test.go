package schema

import (
	"strings"
	"testing"
)

// ux-09b: ddlNotificationQuietAndMutes adds the channel quiet hours, the
// mutes table, the delivered items and the snooze time. No concurrent
// access test: the step is a constant string (the bootstrap lock
// serializes it; the DB test boots twice).

func TestNotificationQuietAndMutesStep_FollowsRuleClass(t *testing.T) {
	steps := trancheSevenMigrations()
	at := -1
	for i, s := range steps {
		if s[0] == "ddlNotificationQuietAndMutes" {
			at = i
		}
	}
	if at < 1 || steps[at-1][0] != "ddlNotificationRuleClass" ||
		steps[at][1] != ddlNotificationQuietAndMutes {
		t.Fatalf("tranche seven %v: want ddlNotificationQuietAndMutes after "+
			"ddlNotificationRuleClass", steps)
	}
}

func TestNotificationQuietAndMutesStep_IsIdempotentAndBounded(t *testing.T) {
	for _, want := range []string{"SET LOCAL lock_timeout",
		"ADD COLUMN IF NOT EXISTS quiet_start text",
		"ADD COLUMN IF NOT EXISTS quiet_end text",
		"ADD COLUMN IF NOT EXISTS quiet_tz text",
		"CREATE TABLE IF NOT EXISTS sage.notification_mutes",
		"CREATE UNIQUE INDEX IF NOT EXISTS notification_mutes_database_class",
		"ADD COLUMN IF NOT EXISTS item_refs text[]",
		"ADD COLUMN IF NOT EXISTS snoozed_at timestamptz",
		"interval '90 days'"} {
		if !strings.Contains(ddlNotificationQuietAndMutes, want) {
			t.Errorf("ddlNotificationQuietAndMutes lacks %q", want)
		}
	}
	for _, bad := range []string{"TRUNCATE", "DELETE FROM", "UPDATE sage.", "DROP ",
		"SET NOT NULL", "RENAME"} {
		if strings.Contains(ddlNotificationQuietAndMutes, bad) {
			t.Errorf("ddlNotificationQuietAndMutes runs or names %q", bad)
		}
	}
	for _, stmt := range classifyStep(ddlNotificationQuietAndMutes) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}
