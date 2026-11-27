package schema

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migration-03: sage.migration_run's verdict CHECK gains 'blocked' (an MCP
// apply_migration the gate refuses records that verdict) and the table
// gains a reason column. The legacy CHECK is found through pg_constraint,
// not by name, so a renamed constraint is replaced too. No concurrent
// access test: the migration runs under Bootstrap's advisory lock.

const legacyMigrationRunVerdict = `
DO $legacy$
DECLARE c record;
BEGIN
    FOR c IN SELECT con.conname FROM pg_constraint con
               JOIN pg_attribute a ON a.attrelid = con.conrelid
                AND a.attnum = ANY (con.conkey)
              WHERE con.conrelid = 'sage.migration_run'::regclass
                AND con.contype = 'c' AND a.attname = 'verdict'
    LOOP
        EXECUTE format('ALTER TABLE sage.migration_run DROP CONSTRAINT %I', c.conname);
    END LOOP;
END
$legacy$;
ALTER TABLE sage.migration_run ADD CONSTRAINT migration_run_legacy_verdict
    CHECK (verdict IN ('pending', 'recommend_only', 'parked', 'promote', 'failed'));
ALTER TABLE sage.migration_run DROP COLUMN IF EXISTS reason;
INSERT INTO sage.migration_run (evidence_id, phase, source_sql_hash, verdict)
VALUES ('migration-03-legacy', 'plan', 'legacy-hash', 'parked');
`

func TestMigrationRunBlockedVerdictMigration(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	requireMigrationRunShape(t, ctx, pool)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.migration_run
			WHERE evidence_id LIKE 'migration-03-%'`)
	})
	if _, err := pool.Exec(ctx, legacyMigrationRunVerdict); err != nil {
		t.Fatalf("simulating the pre-migration-03 schema: %v", err)
	}
	if err := insertMigrationRunVerdict(ctx, pool, "migration-03-early", "blocked"); err == nil {
		t.Fatal("the legacy CHECK accepted 'blocked'; the simulation is wrong")
	}

	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	requireMigrationRunShape(t, ctx, pool)

	var legacyReason string
	if err := pool.QueryRow(ctx, `SELECT reason FROM sage.migration_run
		WHERE evidence_id = 'migration-03-legacy'`).Scan(&legacyReason); err != nil {
		t.Fatalf("reading the legacy row after the migration: %v", err)
	}
	if legacyReason != "" {
		t.Fatalf("legacy row reason = %q, want the '' default", legacyReason)
	}
	if err := insertMigrationRunVerdict(ctx, pool, "migration-03-blocked", "blocked"); err != nil {
		t.Fatalf("inserting a blocked verdict after the migration: %v", err)
	}
	err := insertMigrationRunVerdict(ctx, pool, "migration-03-bogus", "bogus")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("verdict 'bogus' error = %v, want check_violation 23514", err)
	}
}

func requireMigrationRunShape(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT pg_get_constraintdef(con.oid)
		FROM pg_constraint con
		JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = ANY (con.conkey)
		WHERE con.conrelid = 'sage.migration_run'::regclass
		  AND con.contype = 'c' AND a.attname = 'verdict'`)
	if err != nil {
		t.Fatalf("reading verdict CHECKs: %v", err)
	}
	defs := make([]string, 0, 1)
	for rows.Next() {
		var def string
		if err := rows.Scan(&def); err != nil {
			t.Fatalf("scanning verdict CHECK: %v", err)
		}
		defs = append(defs, def)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating verdict CHECKs: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("verdict CHECKs = %q, want exactly one", defs)
	}
	for _, verdict := range []string{"blocked", "pending", "recommend_only", "parked",
		"promote", "failed"} {
		if !strings.Contains(defs[0], "'"+verdict+"'") {
			t.Fatalf("verdict CHECK %q does not allow %q", defs[0], verdict)
		}
	}
	var notNull bool
	var defaultExpr string
	if err := pool.QueryRow(ctx, `SELECT a.attnotnull,
		COALESCE(pg_get_expr(d.adbin, d.adrelid), '')
		FROM pg_attribute a
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE a.attrelid = 'sage.migration_run'::regclass
		  AND a.attname = 'reason' AND NOT a.attisdropped`).Scan(
		&notNull, &defaultExpr); err != nil {
		t.Fatalf("reading sage.migration_run.reason: %v", err)
	}
	if !notNull || !strings.HasPrefix(defaultExpr, "''") {
		t.Fatalf("reason not null=%v default=%q, want NOT NULL DEFAULT ''",
			notNull, defaultExpr)
	}
}

func insertMigrationRunVerdict(
	ctx context.Context, pool *pgxpool.Pool, evidenceID, verdict string,
) error {
	_, err := pool.Exec(ctx, `INSERT INTO sage.migration_run
		(evidence_id, phase, source_sql_hash, verdict) VALUES ($1, 'plan', 'h', $2)`,
		evidenceID, verdict)
	return err
}
