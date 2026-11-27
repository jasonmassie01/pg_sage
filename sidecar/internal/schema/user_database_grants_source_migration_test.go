package schema

import (
	"context"
	"strings"
	"testing"
)

// apiauth-08d: sage.user_database_grants.source says who owns a grant:
// 'manual' (an admin) or 'idp' (the SSO group mapping). Rows written
// before the column existed read 'manual'; bootstrap stays idempotent.

// The global last-step pin lives in TestMigrationStatements_AreTheStepsInOrder;
// later T4 steps land after this one, so this test pins its tranche only.
func TestGrantSourceMigration_StepIsInTrancheFourAndBounded(t *testing.T) {
	steps := trancheFourMigrations()
	found := false
	for _, s := range steps {
		found = found || s[0] == "ddlUserDatabaseGrantsSource"
	}
	if !found {
		t.Fatalf("trancheFourMigrations = %+v, want ddlUserDatabaseGrantsSource", steps)
	}
	for _, want := range []string{"lock_timeout", "ADD COLUMN IF NOT EXISTS source",
		"'manual'", "'idp'"} {
		if !strings.Contains(ddlUserDatabaseGrantsSource, want) {
			t.Errorf("ddlUserDatabaseGrantsSource lacks %q", want)
		}
	}
}

func TestGrantSourceMigration_BootstrapTwiceAndCheck(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool) // a second run is a no-op
	uid := sessionsMigrationUser(t, ctx, pool, "grant-source-mig@example.com")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.users WHERE id = $1`, uid)
	})
	insert := `INSERT INTO sage.user_database_grants (user_id, database_name, role, source)
		VALUES ($1, $2, 'viewer', $3)`
	if _, err := pool.Exec(ctx, insert, uid, "billing", "idp"); err != nil {
		t.Fatalf("inserting an idp grant: %v", err)
	}
	for _, bad := range []string{"IDP", "scim", ""} {
		if _, err := pool.Exec(ctx, insert, uid, "bad-"+bad, bad); err == nil {
			t.Errorf("source %q accepted", bad)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.user_database_grants
		(user_id, database_name, role, source) VALUES ($1, 'nulls', 'viewer', NULL)`,
		uid); err == nil {
		t.Error("a NULL source was accepted")
	}
	var source string
	if err := pool.QueryRow(ctx, `INSERT INTO sage.user_database_grants
		(user_id, database_name, role) VALUES ($1, 'orders', 'operator')
		RETURNING source`, uid).Scan(&source); err != nil || source != "manual" {
		t.Fatalf("default source = %q (%v), want manual", source, err)
	}
}

// The step over a table from before the column: done in a rolled-back
// transaction so other packages sharing the database never see the
// column missing.
func TestGrantSourceMigration_ExistingRowsReadManual(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	uid := sessionsMigrationUser(t, ctx, pool, "grant-source-old@example.com")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.users WHERE id = $1`, uid)
	})
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `ALTER TABLE sage.user_database_grants
		DROP COLUMN source`); err != nil {
		t.Fatalf("dropping source to simulate the old table: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sage.user_database_grants
		(user_id, database_name, role) VALUES ($1, 'orders', 'operator')`, uid); err != nil {
		t.Fatalf("inserting an old-style grant: %v", err)
	}
	for i := 0; i < 2; i++ { // the step is idempotent
		if _, err := tx.Exec(ctx, ddlUserDatabaseGrantsSource); err != nil {
			t.Fatalf("run %d of ddlUserDatabaseGrantsSource: %v", i+1, err)
		}
	}
	var source string
	if err := tx.QueryRow(ctx, `SELECT source FROM sage.user_database_grants
		WHERE user_id = $1 AND database_name = 'orders'`, uid).Scan(&source); err != nil ||
		source != "manual" {
		t.Fatalf("existing grant source = %q (%v), want manual", source, err)
	}
}
