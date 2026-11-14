package schema

import (
	"strings"
	"testing"
)

// alerting-06b: managed_by and config_hash on the notification channels
// and rules are one idempotent T4 step under lock_timeout; existing rows
// read as API rows.
//
// No concurrent access tests: the migration runs inside bootstrap, which
// is serialised by its advisory lock (covered by the bootstrap tests).

func TestNotificationManagedByStepIsRegisteredAndBounded(t *testing.T) {
	four := trancheFourMigrations()
	if len(four) == 0 || four[0][0] != "ddlNotificationManagedBy" {
		t.Fatalf("first T4 step = %v, want ddlNotificationManagedBy", four)
	}
	for _, want := range []string{"SET LOCAL lock_timeout",
		"ALTER TABLE sage.notification_channels", "ALTER TABLE sage.notification_rules",
		"ADD COLUMN IF NOT EXISTS managed_by text NOT NULL DEFAULT 'api'",
		"CHECK (managed_by IN ('api', 'config'))", "ADD COLUMN IF NOT EXISTS config_hash text",
		"WHERE managed_by = 'config'"} {
		if !strings.Contains(ddlNotificationManagedBy, want) {
			t.Errorf("the step lacks %q", want)
		}
	}
	for _, stmt := range classifyStep(ddlNotificationManagedBy) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not prechecked; it would run on every boot", stmt.sql)
		}
	}
}

// Integration: bootstrap runs twice cleanly on a fresh server; a row
// written without managed_by is an API row, and the check refuses others.
func TestNotificationManagedByBootstrapTwice(t *testing.T) {
	pool := freshPool(t, "notification_managed")
	ctx := t.Context()
	for run := 1; run <= 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	var managedBy string
	if err := pool.QueryRow(ctx, `INSERT INTO sage.notification_channels (name, type)
		VALUES ('a06b-api', 'slack') RETURNING managed_by`).Scan(&managedBy); err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	if managedBy != "api" {
		t.Fatalf("default managed_by = %q, want api", managedBy)
	}
	_, err := pool.Exec(ctx, `INSERT INTO sage.notification_rules
		(event, managed_by) VALUES ('finding_opened', 'yaml')`)
	if err == nil || !strings.Contains(err.Error(), "check") {
		t.Fatalf("managed_by 'yaml' err = %v, want a check violation", err)
	}
}
