package schema

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// review-04a on a real server: bootstrap twice is clean and the trigger
// queue refuses what it must. No concurrent-writer test here: Bootstrap
// holds its advisory lock; the queue's claims are tested in
// internal/review/trigger.

func TestReviewTriggersStep_IsTheLastT6StepIdempotentAndBounded(t *testing.T) {
	// review-04b appended ddlReviewFleet after this step, so the pin is its
	// place (just before ddlReviewFleet), not "last".
	steps := trancheSixMigrations()
	at := -1
	for i, s := range steps {
		if s[0] == "ddlReviewTriggers" && s[1] == ddlReviewTriggers {
			at = i
		}
	}
	if at < 0 || at+1 >= len(steps) || steps[at+1][0] != "ddlReviewFleet" {
		t.Fatalf("T6 steps %v, want ddlReviewTriggers just before ddlReviewFleet", steps)
	}
	for _, want := range []string{"SET LOCAL lock_timeout = '5s'",
		"CREATE TABLE IF NOT EXISTS sage.review_trigger (",
		"CREATE TABLE IF NOT EXISTS sage.review_trigger_state (",
		"jsonb_typeof(detail) = 'object'", "'calendar'", "'pre_upgrade'",
		"REFERENCES sage.review_run (id) ON DELETE SET NULL",
		"CREATE UNIQUE INDEX IF NOT EXISTS review_trigger_dedupe"} {
		if !strings.Contains(ddlReviewTriggers, want) {
			t.Errorf("ddlReviewTriggers lacks %q", want)
		}
	}
	for _, bad := range []string{"UPDATE sage.", "DELETE FROM", "DROP TABLE"} {
		if strings.Contains(ddlReviewTriggers, bad) {
			t.Errorf("ddlReviewTriggers runs %q", bad)
		}
	}
	for _, stmt := range classifyStep(ddlReviewTriggers) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}

func TestReviewTriggersMigration_BootstrapTwiceAndChecked(t *testing.T) {
	pool := freshSchemaPool(t, "reviewtriggers")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	var ledger int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.schema_migrations
		WHERE id = 'ddlReviewTriggers'`).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("ledger rows for ddlReviewTriggers: %d (%v), want 1", ledger, err)
	}
	bad := []string{
		`INSERT INTO sage.review_trigger (database_name, kind, dedupe_key)
		 VALUES ('shop', 'weekly', 'k')`,
		`INSERT INTO sage.review_trigger (database_name, kind, dedupe_key, detail)
		 VALUES ('shop', 'calendar', 'k', '[]')`,
		`INSERT INTO sage.review_trigger (database_name, kind, dedupe_key, state)
		 VALUES ('shop', 'calendar', 'k', 'claimed')`,
		`INSERT INTO sage.review_trigger (database_name, kind, dedupe_key, state)
		 VALUES ('shop', 'calendar', 'k', 'coalesced')`,
		`INSERT INTO sage.review_trigger (database_name, kind, dedupe_key)
		 VALUES ('', 'calendar', 'k')`,
		`INSERT INTO sage.review_trigger_state (database_name, source, facts)
		 VALUES ('shop', 'instance_change', '"x"')`,
	}
	for _, sql := range bad {
		_, err := pool.Exec(ctx, sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Errorf("%s: err %v, want a check violation", sql, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.review_trigger
		(database_name, kind, dedupe_key) VALUES ('shop', 'calendar', 'k')`); err != nil {
		t.Fatalf("valid trigger refused: %v", err)
	}
	_, err := pool.Exec(ctx, `INSERT INTO sage.review_trigger
		(database_name, kind, dedupe_key) VALUES ('shop', 'calendar', 'k')`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("duplicate key: %v, want a unique violation", err)
	}
}
