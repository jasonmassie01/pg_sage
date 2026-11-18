package schema

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// review-04b on a real server: bootstrap twice is clean, the fleet ledger
// refuses what it must, and the trigger queue takes the fleet_outlier
// kind. No concurrent-writer test here: Bootstrap holds its advisory lock;
// the ledger's state rules are tested in internal/review/fleet.

// fleetpol-01a's ddlPolicySets merged after this step, so the pin is its
// place: right after ddlReviewTriggers, whose table it alters.
func TestReviewFleetStep_IsPlacedIdempotentAndBounded(t *testing.T) {
	steps := trancheSixMigrations()
	at := stepIndex(steps, "ddlReviewFleet")
	if at < 1 || steps[at][1] != ddlReviewFleet || steps[at-1][0] != "ddlReviewTriggers" {
		t.Fatalf("T6 steps %v, want ddlReviewTriggers then ddlReviewFleet", steps)
	}
	for _, want := range []string{"SET LOCAL lock_timeout = '5s'",
		"CREATE TABLE IF NOT EXISTS sage.review_fleet_pass (",
		"CREATE TABLE IF NOT EXISTS sage.review_fleet_member (",
		"REFERENCES sage.review_fleet_pass (id) ON DELETE CASCADE",
		"'rollup', 'triaged', 'deep_scheduled', 'reviewed'", "'fleet_outlier'",
		"review_trigger_kind_check"} {
		if !strings.Contains(ddlReviewFleet, want) {
			t.Errorf("ddlReviewFleet lacks %q", want)
		}
	}
	for _, bad := range []string{"UPDATE sage.", "DELETE FROM", "DROP TABLE"} {
		if strings.Contains(ddlReviewFleet, bad) {
			t.Errorf("ddlReviewFleet runs %q", bad)
		}
	}
	for _, stmt := range classifyStep(ddlReviewFleet) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}

func TestReviewFleetMigration_BootstrapTwiceAndChecked(t *testing.T) {
	pool := freshSchemaPool(t, "reviewfleet")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	var ledger, kindChecks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.schema_migrations
		WHERE id = 'ddlReviewFleet'`).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("ledger rows for ddlReviewFleet: %d (%v), want 1", ledger, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
		WHERE conrelid = 'sage.review_trigger'::regclass AND contype = 'c'
		AND pg_get_constraintdef(oid) LIKE '%kind%'`).Scan(&kindChecks); err != nil ||
		kindChecks != 1 {
		t.Fatalf("kind checks on sage.review_trigger: %d (%v), want exactly 1", kindChecks, err)
	}
	checkFleetLedgerChecks(t, pool)
}

func checkFleetLedgerChecks(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	var pass int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.review_fleet_pass (fleet_scope, period,
		members) VALUES ('meta', '2026-11', 2) RETURNING id`).Scan(&pass); err != nil {
		t.Fatalf("valid pass refused: %v", err)
	}
	for _, sql := range []string{
		`INSERT INTO sage.review_fleet_pass (fleet_scope, period, members)
		 VALUES ('meta', '2026-13x', 1)`,
		`INSERT INTO sage.review_fleet_pass (fleet_scope, period, members, status)
		 VALUES ('meta', '2026-11', 1, 'done')`,
		`INSERT INTO sage.review_fleet_pass (fleet_scope, period, members)
		 VALUES ('', '2026-11', 1)`,
		`INSERT INTO sage.review_fleet_member (pass_id, database_name, rollup, state)
		 VALUES (` + strconv.FormatInt(pass, 10) + `, 'a', '{}', 'deep_scheduled')`,
		`INSERT INTO sage.review_fleet_member (pass_id, database_name, rollup)
		 VALUES (` + strconv.FormatInt(pass, 10) + `, 'a', '[]')`,
		`INSERT INTO sage.review_fleet_member (pass_id, database_name, rollup, state)
		 VALUES (` + strconv.FormatInt(pass, 10) + `, 'a', '{}', 'done')`,
	} {
		_, err := pool.Exec(ctx, sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Errorf("%s: err %v, want a check violation", sql, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.review_trigger (database_name, kind,
		dedupe_key) VALUES ('shop', 'fleet_outlier', 'fleet:2026-11')`); err != nil {
		t.Fatalf("a fleet_outlier trigger was refused: %v", err)
	}
	_, err := pool.Exec(ctx, `INSERT INTO sage.review_trigger (database_name, kind,
		dedupe_key) VALUES ('shop', 'weekly', 'k')`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("an unknown kind: %v, want a check violation", err)
	}
}
