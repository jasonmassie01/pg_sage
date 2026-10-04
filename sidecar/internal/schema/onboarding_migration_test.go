package schema

import (
	"context"
	"testing"
)

// Five-minute time to value: sage.first_look keeps each database's
// catalog-only first look reports; sage.onboarding keeps one row per
// database (install kind, first look, first finding). Additive and
// idempotent.

func TestOnboardingMigrationIsIdempotent(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 3; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	for _, table := range []string{"first_look", "onboarding"} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
			WHERE table_schema = 'sage' AND table_name = $1`, table).Scan(&n); err != nil ||
			n != 1 {
			t.Fatalf("table %s: %d (%v)", table, n, err)
		}
	}
	if !containsString(migrationStatements(), ddlOnboarding) {
		t.Fatal("the onboarding migration is not registered")
	}
}

func TestOnboardingTableConstraints(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	clean := func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.onboarding WHERE database_name LIKE 'mig_%'")
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.first_look WHERE database_name LIKE 'mig_%'")
	}
	clean()
	t.Cleanup(clean)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.onboarding (database_name, install_kind)
		VALUES ('mig_a', 'new')`); err != nil {
		t.Fatalf("valid onboarding row: %v", err)
	}
	for _, sql := range []string{
		`INSERT INTO sage.onboarding (database_name, install_kind) VALUES ('mig_a', 'new')`,
		`INSERT INTO sage.onboarding (database_name, install_kind) VALUES ('mig_b', 'maybe')`,
		`INSERT INTO sage.onboarding (database_name, install_kind) VALUES ('', 'new')`,
		`INSERT INTO sage.onboarding (database_name, install_kind, ttff_ms)
			VALUES ('mig_c', 'new', -1)`,
		`INSERT INTO sage.first_look (database_name, started_at, finished_at)
			VALUES ('', now(), now())`,
		`INSERT INTO sage.first_look (database_name, started_at, finished_at, duration_ms)
			VALUES ('mig_d', now(), now(), -5)`,
	} {
		if _, err := pool.Exec(ctx, sql); err == nil {
			t.Fatalf("invalid row accepted: %s", sql)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.first_look (database_name, started_at,
		finished_at) VALUES ('mig_e', now(), now())`); err != nil {
		t.Fatalf("valid first look row with defaults: %v", err)
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
