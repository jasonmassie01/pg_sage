package schema

import (
	"context"
	"strings"
	"testing"
)

// ux-01b: ddlUserInboxSeenAt adds sage.users.inbox_seen_at, the time a
// person last saw the Inbox. No concurrent access test here: the step is a
// constant string run under the bootstrap lock; the GREATEST race is
// api.TestInboxSeenDB_ConcurrentMarksKeepTheLaterTime.

func TestUserInboxSeenAtStep_IsLastBoundedAndIdempotent(t *testing.T) {
	steps := trancheFiveMigrations()
	if len(steps) < 2 || steps[len(steps)-1][0] != "ddlUserInboxSeenAt" ||
		steps[len(steps)-1][1] != ddlUserInboxSeenAt {
		t.Fatalf("T5 steps = %v, want ddlUserInboxSeenAt last", steps)
	}
	if steps[len(steps)-2][0] != "ddlWorkloadPseudonymKey" {
		t.Fatalf("ddlUserInboxSeenAt follows %s, want ddlWorkloadPseudonymKey",
			steps[len(steps)-2][0])
	}
	for _, want := range []string{"SET LOCAL lock_timeout",
		"ALTER TABLE sage.users", "ADD COLUMN IF NOT EXISTS inbox_seen_at timestamptz"} {
		if !strings.Contains(ddlUserInboxSeenAt, want) {
			t.Errorf("the step lacks %q", want)
		}
	}
	if strings.Contains(strings.ToUpper(ddlUserInboxSeenAt), "DEFAULT") ||
		strings.Contains(strings.ToUpper(ddlUserInboxSeenAt), "NOT NULL") {
		t.Error("inbox_seen_at must be a nullable column with no default (NULL is a first visit)")
	}
	for _, stmt := range classifyStep(ddlUserInboxSeenAt) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}

// Integration: two bootstraps leave one nullable timestamptz column, and
// an existing user keeps NULL (a first visit) until the Inbox marks it.
func TestUserInboxSeenAtMigration_BootstrapTwice(t *testing.T) {
	pool := freshSchemaPool(t, "inboxseen")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	var typ string
	var notNull bool
	if err := pool.QueryRow(ctx, `SELECT format_type(atttypid, atttypmod), attnotnull
		FROM pg_attribute WHERE attrelid = 'sage.users'::regclass
		AND attname = 'inbox_seen_at' AND NOT attisdropped`).Scan(&typ, &notNull); err != nil {
		t.Fatalf("read inbox_seen_at: %v", err)
	}
	if typ != "timestamp with time zone" || notNull {
		t.Fatalf("inbox_seen_at is %s (not null %v), want nullable timestamptz", typ, notNull)
	}
	var seen *string
	if err := pool.QueryRow(ctx, `INSERT INTO sage.users (email, password)
		VALUES ('inbox@example.com', 'x') RETURNING inbox_seen_at::text`).Scan(&seen); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if seen != nil {
		t.Fatalf("a new user has inbox_seen_at %q, want NULL", *seen)
	}
}
