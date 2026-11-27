package schema

import (
	"strings"
	"testing"
)

// alerting-06: the notification outbox and per-channel alert state are
// one idempotent T3 step under lock_timeout.
//
// No concurrent access tests: the migration runs inside bootstrap, which
// is serialised by its advisory lock (covered by the bootstrap tests).

func TestNotificationOutboxStepIsRegisteredAndBounded(t *testing.T) {
	var found []string
	for _, step := range migrationSteps() {
		if strings.Contains(step.sql, "sage.notification_outbox (") {
			found = append(found, step.name)
		}
	}
	if len(found) != 1 || found[0] != "ddlNotificationOutbox" {
		t.Fatalf("steps creating the outbox = %v, want only ddlNotificationOutbox", found)
	}
	for _, want := range []string{"SET LOCAL lock_timeout",
		"CREATE TABLE IF NOT EXISTS sage.notification_outbox",
		"CREATE TABLE IF NOT EXISTS sage.notification_state",
		"PRIMARY KEY (database, dedup_key, channel_id)", "ON DELETE CASCADE"} {
		if !strings.Contains(ddlNotificationOutbox, want) {
			t.Errorf("the step lacks %q", want)
		}
	}
}

// Integration: bootstrap runs twice cleanly on a fresh server, and the
// status check refuses an unknown status.
func TestNotificationOutboxBootstrapTwice(t *testing.T) {
	pool := freshPool(t, "notification_outbox")
	ctx := t.Context()
	for run := 1; run <= 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_class
		WHERE oid IN (to_regclass('sage.notification_outbox'),
		              to_regclass('sage.notification_state'),
		              to_regclass('sage.idx_notification_outbox_due'),
		              to_regclass('sage.idx_notification_state_open'))`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("outbox relations = %d, want 4", n)
	}
	var chID int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.notification_channels (name, type)
		VALUES ('a06', 'slack') RETURNING id`).Scan(&chID); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, `INSERT INTO sage.notification_outbox
		(dedup_key, channel_id, event, payload, status) VALUES ('k', $1, 'x', '{}', 'lost')`,
		chID)
	if err == nil || !strings.Contains(err.Error(), "check") {
		t.Fatalf("unknown status err = %v, want a check violation", err)
	}
}
