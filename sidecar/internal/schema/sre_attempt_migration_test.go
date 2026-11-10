package schema

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sre-01: investigations under one idempotency key are numbered attempts.
// Rows stored before the migration become attempt 1; the key is unique
// per attempt instead of over all time; the event chain admits
// attempts_exhausted.

const (
	attemptDeployment = "51515151-5151-4151-8151-515151515151"
	attemptDatabase   = "52525252-5252-4252-8252-525252525252"
	attemptLegacyID   = "53535353-5353-4353-8353-535353535353"
)

// legacySREAttempts is the schema before sre-01: no attempt columns and
// the all-time unique idempotency index.
const legacySREAttempts = `
DROP INDEX IF EXISTS sage.sre_investigation_attempt;
DROP INDEX IF EXISTS sage.sre_investigation_supersedes;
ALTER TABLE sage.sre_investigations DROP COLUMN IF EXISTS supersedes,
    DROP COLUMN IF EXISTS attempt;
CREATE UNIQUE INDEX IF NOT EXISTS sre_investigation_idempotency
    ON sage.sre_investigations (deployment_id, database_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
`

func attemptCleanup(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	clean := func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM sage.sre_investigations WHERE deployment_id = $1`,
			attemptDeployment)
		_, _ = pool.Exec(bg, `DELETE FROM sage.sre_database_bindings WHERE deployment_id = $1`,
			attemptDeployment)
	}
	clean()
	t.Cleanup(clean)
}

// insertAttempt stores a keyed investigation under the attempt columns;
// legacy omits them, as a row written before sre-01 was.
func insertAttempt(ctx context.Context, pool *pgxpool.Pool, id string, attempt int,
	supersedes any, legacy bool) error {
	cols, vals := ", attempt, supersedes", ", $4, $5"
	args := []any{attemptDeployment, attemptDatabase, id, attempt, supersedes}
	if legacy {
		cols, vals, args = "", "", args[:3]
	}
	_, err := pool.Exec(ctx, `INSERT INTO sage.sre_investigations
		(deployment_id, database_id, id, source_case_id, trigger_kind, trigger_fingerprint,
		 idempotency_key, state, expires_at, subject`+cols+`)
		VALUES ($1, $2, $3::uuid, 'case:sre01', 'lock_contention',
		 sha256(convert_to($3::uuid::text, 'UTF8')),
		 'incident:sre01', 'expired', clock_timestamp() + interval '1 minute',
		 'incident sre01'`+vals+`)`, args...)
	return err
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func indexExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes
		WHERE schemaname = 'sage' AND indexname = $1`, name).Scan(&n); err != nil {
		t.Fatalf("read index %s: %v", name, err)
	}
	return n == 1
}

// Migration (PG 14-18): an upgrade with existing rows keeps them at
// attempt 1, and bootstrap runs twice cleanly afterwards.
func TestSREAttemptMigration_UpgradeKeepsRowsAtAttemptOne(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	attemptCleanup(t, pool)
	if _, err := pool.Exec(ctx, legacySREAttempts); err != nil {
		t.Fatalf("simulating the pre-sre-01 schema: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.sre_database_bindings
		(deployment_id, database_id, runtime_key, identity_strength, cluster_epoch)
		VALUES ($1, $2, 'sre01-migration', 'configured', 'e1')`,
		attemptDeployment, attemptDatabase); err != nil {
		t.Fatalf("binding: %v", err)
	}
	if err := insertAttempt(ctx, pool, attemptLegacyID, 0, nil, true); err != nil {
		t.Fatalf("legacy row: %v", err)
	}
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	var attempt int
	var supersedes *string
	if err := pool.QueryRow(ctx, `SELECT attempt, supersedes::text
		FROM sage.sre_investigations WHERE deployment_id = $1 AND id = $2`,
		attemptDeployment, attemptLegacyID).Scan(&attempt, &supersedes); err != nil {
		t.Fatalf("read legacy row: %v", err)
	}
	if attempt != 1 || supersedes != nil {
		t.Fatalf("legacy row = attempt %d supersedes %v, want attempt 1 and NULL",
			attempt, supersedes)
	}
	if indexExists(t, ctx, pool, "sre_investigation_idempotency") ||
		!indexExists(t, ctx, pool, "sre_investigation_attempt") ||
		!indexExists(t, ctx, pool, "sre_investigation_supersedes") {
		t.Fatal("indexes after upgrade: want sre_investigation_attempt and " +
			"sre_investigation_supersedes, and no sre_investigation_idempotency")
	}
}

// The database enforces the attempt rules: one row per key and attempt,
// attempts 1-5, and a predecessor exactly when attempt > 1.
func TestSREAttemptMigration_Constraints(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	attemptCleanup(t, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.sre_database_bindings
		(deployment_id, database_id, runtime_key, identity_strength, cluster_epoch)
		VALUES ($1, $2, 'sre01-constraints', 'configured', 'e1')`,
		attemptDeployment, attemptDatabase); err != nil {
		t.Fatalf("binding: %v", err)
	}
	const second = "54545454-5454-4454-8454-545454545454"
	if err := insertAttempt(ctx, pool, attemptLegacyID, 1, nil, false); err != nil {
		t.Fatalf("attempt 1: %v", err)
	}
	if err := insertAttempt(ctx, pool, second, 2, attemptLegacyID, false); err != nil {
		t.Fatalf("attempt 2 under the same key: %v", err)
	}
	cases := []struct {
		name       string
		attempt    int
		supersedes any
		want       string
	}{
		{"a second attempt 2", 2, attemptLegacyID, "23505"},
		{"a second attempt 1", 1, nil, "23505"},
		{"attempt 6", 6, second, "23514"},
		{"attempt 0", 0, nil, "23514"},
		{"attempt 3 without a predecessor", 3, nil, "23514"},
		{"attempt 1 with a predecessor", 1, second, "23514"},
	}
	for i, c := range cases {
		id := fmt.Sprintf("5555555%d-5555-4555-8555-555555555555", i)
		err := insertAttempt(ctx, pool, id, c.attempt, c.supersedes, false)
		if got := sqlState(err); got != c.want {
			t.Errorf("%s: SQLSTATE %q (%v), want %s", c.name, got, err, c.want)
		}
	}
}

func TestSREAttemptMigration_EventChainAdmitsAttemptsExhausted(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	if allowed := eventTypeCheck(t); !allowed["attempts_exhausted"] {
		t.Fatalf("sre_events check lacks attempts_exhausted: %v", allowed)
	}
}
