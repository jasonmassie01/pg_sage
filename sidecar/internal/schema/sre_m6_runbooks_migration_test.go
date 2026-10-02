package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Sage SRE M6 runbooks and incident memory: the migration is idempotent,
// a runbook version's content can never change, a signature must bind the
// version's own content hash, and run history and outcomes go with their
// investigation when retention deletes it.

const (
	m6Dep = "51111111-1111-4111-8111-111111111111"
	m6DB  = "52222222-2222-4222-8222-222222222222"
	m6Inv = "53333333-3333-4333-8333-333333333333"
	m6RB  = "54444444-4444-4444-8444-444444444444"
)

func TestSREMigrationM6_RunbookTablesAndGuards(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	for _, table := range []string{"sre_runbooks", "sre_runbook_versions",
		"sre_runbook_runs", "sre_investigation_outcomes"} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('sage.' || $1) IS NOT NULL`,
			table).Scan(&ok); err != nil || !ok {
			t.Fatalf("table sage.%s missing (%v)", table, err)
		}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	seedM6(t, ctx, tx)
	checkM6Guards(t, ctx, tx)
	if _, err := tx.Exec(ctx, `DELETE FROM sage.sre_investigations WHERE id = $1`,
		m6Inv); err != nil {
		t.Fatalf("delete investigation: %v", err)
	}
	var runs, outcomes int
	if err := tx.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM sage.sre_runbook_runs WHERE investigation_id = $1),
		(SELECT count(*) FROM sage.sre_investigation_outcomes WHERE investigation_id = $1)`,
		m6Inv).Scan(&runs, &outcomes); err != nil || runs != 0 || outcomes != 0 {
		t.Fatalf("after delete: %d runs, %d outcomes (%v); want them cascaded", runs,
			outcomes, err)
	}
}

// checkM6Guards: content updates and deletes are refused, a signature must
// bind the content hash, and a signed version cannot change.
func checkM6Guards(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	guarded := []string{
		`UPDATE sage.sre_runbook_versions SET name = 'x' WHERE runbook_id = '` + m6RB + `'`,
		`DELETE FROM sage.sre_runbook_versions WHERE runbook_id = '` + m6RB + `'`,
	}
	for i, stmt := range guarded {
		if err := m6Savepoint(ctx, tx, stmt); err == nil ||
			!strings.Contains(err.Error(), "runbook") {
			t.Errorf("guarded statement %d: err = %v, want refused", i, err)
		}
	}
	mismatch := `UPDATE sage.sre_runbook_versions SET signed_by = 'user:1',
		signer_role = 'admin', signed_at = now(), signed_hash = '\x` +
		strings.Repeat("ff", 32) + `' WHERE runbook_id = '` + m6RB + `'`
	if err := m6Savepoint(ctx, tx, mismatch); err == nil {
		t.Error("a signature over another hash was stored")
	}
	sign := `UPDATE sage.sre_runbook_versions SET signed_by = 'user:1',
		signer_role = 'admin', signed_at = now(), signed_hash = content_hash
		WHERE runbook_id = '` + m6RB + `'`
	if err := m6Savepoint(ctx, tx, sign); err != nil {
		t.Fatalf("signing the content hash: %v", err)
	}
	if err := m6Savepoint(ctx, tx, sign); err == nil {
		t.Error("a signed version was changed")
	}
}

func seedM6(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	hash := `'\x` + strings.Repeat("ab", 32) + `'`
	for _, stmt := range []string{
		`INSERT INTO sage.sre_database_bindings (deployment_id, database_id, runtime_key,
			identity_strength, cluster_epoch) VALUES ('` + m6Dep + `', '` + m6DB + `',
			'm6-test', 'configured', 'e1')`,
		`INSERT INTO sage.sre_investigations (deployment_id, database_id, id,
			source_case_id, trigger_kind, trigger_fingerprint, state, expires_at)
			VALUES ('` + m6Dep + `', '` + m6DB + `', '` + m6Inv + `', 'case', 'lock_blocking',
			'\x` + strings.Repeat("01", 32) + `', 'concluded', now() + interval '1 hour')`,
		`INSERT INTO sage.sre_runbooks (deployment_id, database_id, id, created_by)
			VALUES ('` + m6Dep + `', '` + m6DB + `', '` + m6RB + `', 'user:2')`,
		`INSERT INTO sage.sre_runbook_versions (deployment_id, database_id, runbook_id,
			version, name, definition, content_hash, source, created_by)
			VALUES ('` + m6Dep + `', '` + m6DB + `', '` + m6RB + `', 1, 'rb', '{}', ` + hash +
			`, 'manual', 'user:2')`,
		`INSERT INTO sage.sre_runbook_runs (deployment_id, database_id, investigation_id,
			runbook_id, version, content_hash, outcome, path, probes)
			VALUES ('` + m6Dep + `', '` + m6DB + `', '` + m6Inv + `', '` + m6RB + `', 1, ` +
			hash + `, 'completed', '["a"]', 1)`,
		`INSERT INTO sage.sre_investigation_outcomes (deployment_id, database_id,
			investigation_id, id, verdict, actor)
			VALUES ('` + m6Dep + `', '` + m6DB + `', '` + m6Inv + `',
			'55555555-5555-4555-8555-555555555555', 'confirmed', 'user:4')`,
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}
}

// m6Savepoint runs stmt in a savepoint, rolling back to it on error.
func m6Savepoint(ctx context.Context, tx pgx.Tx, stmt string) error {
	if _, err := tx.Exec(ctx, "SAVEPOINT m6"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, stmt); err != nil {
		_, _ = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT m6")
		return err
	}
	_, err := tx.Exec(ctx, "RELEASE SAVEPOINT m6")
	return err
}
