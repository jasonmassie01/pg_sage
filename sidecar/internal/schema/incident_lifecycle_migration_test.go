package schema

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestIncidentLifecycleMigration_ColumnsFKIdempotent proves the R04 /
// SURF-19 columns exist after Bootstrap, the recurrence FK clears on
// delete, and re-running Bootstrap is a no-op.
func TestIncidentLifecycleMigration_ColumnsFKIdempotent(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool) // idempotent

	for _, col := range []string{
		"resolved_by", "resolution_reason", "identity_key",
		"previous_incident_id",
	} {
		var n int
		err := pool.QueryRow(ctx, `SELECT count(*)
			FROM information_schema.columns
			WHERE table_schema = 'sage' AND table_name = 'incidents'
			  AND column_name = $1`, col).Scan(&n)
		if err != nil || n != 1 {
			t.Fatalf("column %s: count=%d err=%v", col, n, err)
		}
	}

	var parent, child string
	if err := pool.QueryRow(ctx, `INSERT INTO sage.incidents
		(severity, root_cause, source, database_name)
		VALUES ('warning', 'p', 'deterministic', 'mig_test')
		RETURNING id::text`).Scan(&parent); err != nil {
		t.Fatalf("insert parent: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.incidents WHERE database_name = 'mig_test'")
	})
	if err := pool.QueryRow(ctx, `INSERT INTO sage.incidents
		(severity, root_cause, source, database_name, previous_incident_id)
		VALUES ('warning', 'c', 'deterministic', 'mig_test', $1)
		RETURNING id::text`, parent).Scan(&child); err != nil {
		t.Fatalf("insert child: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"DELETE FROM sage.incidents WHERE id = $1", parent); err != nil {
		t.Fatalf("delete parent: %v", err)
	}
	var prev *string
	if err := pool.QueryRow(ctx, `SELECT previous_incident_id::text
		FROM sage.incidents WHERE id = $1`, child).Scan(&prev); err != nil {
		t.Fatalf("read child: %v", err)
	}
	if prev != nil {
		t.Errorf("previous_incident_id = %v, want NULL after parent delete",
			*prev)
	}

	_, err := pool.Exec(ctx, `INSERT INTO sage.incidents
		(severity, root_cause, source, database_name, previous_incident_id)
		VALUES ('warning', 'x', 'deterministic', 'mig_test',
		        '00000000-0000-4000-8000-000000000000')`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" ||
		!strings.Contains(pgErr.ConstraintName, "previous_incident") {
		t.Errorf("dangling previous_incident_id err = %v, want FK 23503", err)
	}
}
