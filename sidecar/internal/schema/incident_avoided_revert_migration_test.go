package schema

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// earned-09: sage.incident_avoided gains a nullable reverted_at and two
// narrowed CHECKs (only the kinds and severity a producer writes).

func incidentRevertDB(t *testing.T, label string) (*pgxpool.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, label))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	return pool, ctx
}

// producedChecks reports whether each narrowed constraint is validated.
func producedChecks(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[string]bool {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT conname, convalidated FROM pg_constraint
		WHERE conrelid = 'sage.incident_avoided'::regclass
		  AND conname IN ('incident_avoided_kind_produced',
		                  'incident_avoided_severity_produced')`)
	if err != nil {
		t.Fatalf("read constraints: %v", err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var name string
		var valid bool
		if err := rows.Scan(&name, &valid); err != nil {
			t.Fatal(err)
		}
		got[name] = valid
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestIncidentAvoidedRevertMigrationIsIdempotent(t *testing.T) {
	pool, ctx := incidentRevertDB(t, "incident_revert_idem")
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	var typ string
	var notNull bool
	if err := pool.QueryRow(ctx, `SELECT format_type(atttypid, atttypmod), attnotnull
		FROM pg_attribute WHERE attrelid = 'sage.incident_avoided'::regclass
		  AND attname = 'reverted_at' AND NOT attisdropped`).Scan(&typ, &notNull); err != nil {
		t.Fatalf("reverted_at column: %v", err)
	}
	if typ != "timestamp with time zone" || notNull {
		t.Fatalf("reverted_at is %s (not null %v), want nullable timestamptz", typ, notNull)
	}
	got := producedChecks(t, ctx, pool)
	if len(got) != 2 || !got["incident_avoided_kind_produced"] ||
		!got["incident_avoided_severity_produced"] {
		t.Fatalf("narrowed constraints = %v, want both present and validated", got)
	}
}

// An install with a row holding a retired value keeps it as evidence: the
// constraints stay NOT VALID (bootstrap never fails on old rows) and still
// refuse new ones; once the row is gone the next bootstrap validates them.
func TestIncidentAvoidedRevertMigrationKeepsLegacyRows(t *testing.T) {
	pool, ctx := incidentRevertDB(t, "incident_revert_legacy")
	if _, err := pool.Exec(ctx, `ALTER TABLE sage.incident_avoided
		DROP CONSTRAINT incident_avoided_kind_produced,
		DROP CONSTRAINT incident_avoided_severity_produced`); err != nil {
		t.Fatalf("simulate a pre-earned-09 install: %v", err)
	}
	if err := tryLegacyIncident(ctx, pool, "legacy", "lock_storm", "prevented"); err != nil {
		t.Fatalf("insert a legacy lock_storm/prevented incident: %v", err)
	}
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap over a legacy row: %v", err)
	}
	got := producedChecks(t, ctx, pool)
	if len(got) != 2 || got["incident_avoided_kind_produced"] ||
		got["incident_avoided_severity_produced"] {
		t.Fatalf("constraints over a legacy row = %v, want both NOT VALID", got)
	}
	if err := tryLegacyIncident(ctx, pool, "new", "lock_storm", "near_miss"); err == nil {
		t.Fatal("a new lock_storm row was accepted by the NOT VALID constraint")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM sage.incident_avoided
		WHERE evidence_id = 'legacy'`); err != nil {
		t.Fatal(err)
	}
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap after the legacy row is gone: %v", err)
	}
	got = producedChecks(t, ctx, pool)
	if !got["incident_avoided_kind_produced"] || !got["incident_avoided_severity_produced"] {
		t.Fatalf("constraints after cleanup = %v, want both validated", got)
	}
}

// tryLegacyIncident inserts one incident with its evidence graph.
func tryLegacyIncident(
	ctx context.Context, pool *pgxpool.Pool, evidence, kind, severity string,
) error {
	_, err := pool.Exec(ctx, `WITH d AS (
		INSERT INTO sage.decision (feature, intent, verdict, risk_tier, reason, evidence_id)
		VALUES ('freeze', 'legacy', 'execute', 'safe', 'test', $1) RETURNING id
	), a AS (
		INSERT INTO sage.action_log (action_type, sql_executed, outcome)
		VALUES ('vacuum', 'VACUUM t', 'success') RETURNING id
	), v AS (
		INSERT INTO sage.verification (decision_id, action_log_id, criterion, baseline,
		  minimum_samples, next_evaluation_at, hard_deadline_at, verdict, completed_at)
		SELECT d.id, a.id, '{}'::jsonb, '{}'::jsonb, 1, now(), now(), 'success', now()
		FROM d, a
		RETURNING id, decision_id, action_log_id
	)
	INSERT INTO sage.incident_avoided (kind, severity, credited_minutes, evidence_id,
	  decision_id, action_log_id, verification_id)
	SELECT $2, $3, 480, $1, v.decision_id, v.action_log_id, v.id FROM v`,
		evidence, kind, severity)
	return err
}
