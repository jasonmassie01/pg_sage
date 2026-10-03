package schema

import (
	"slices"
	"strings"
	"testing"
)

// sage.optimizer_rejection remembers HypoPG what-if rejections of optimizer
// candidates: one row per database, table and normalized shape. The
// migration is idempotent and safe to re-run.

func TestOptimizerRejectionMigrationIsRegistered(t *testing.T) {
	if !slices.Contains(migrationStatements(), ddlOptimizerRejection) {
		t.Fatal("ddlOptimizerRejection is not registered in migrationStatements")
	}
	if !strings.Contains(ddlOptimizerRejection, "IF NOT EXISTS") {
		t.Fatal("the migration must be idempotent")
	}
}

func TestOptimizerRejectionMigrationIsIdempotent(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	for _, col := range []string{"database_name", "schema_name", "table_name", "shape_hash",
		"method", "key_cols", "predicate", "include_cols", "ddl", "improvement_pct",
		"min_improvement_pct", "reason", "workload", "row_estimate", "measure_count",
		"first_measured_at", "measured_at"} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
			WHERE table_schema = 'sage' AND table_name = 'optimizer_rejection'
			  AND column_name = $1`, col).Scan(&n); err != nil || n != 1 {
			t.Errorf("sage.optimizer_rejection.%s missing (%v)", col, err)
		}
	}
	var ok bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('sage.idx_optimizer_rejection_measured')
		IS NOT NULL`).Scan(&ok); err != nil || !ok {
		t.Errorf("index sage.idx_optimizer_rejection_measured missing (%v)", err)
	}
}

func TestOptimizerRejectionConstraints(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	tag := "rejmig_" + strings.ReplaceAll(t.Name(), "/", "_")
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.optimizer_rejection WHERE schema_name = $1", tag)
	})
	insert := `INSERT INTO sage.optimizer_rejection (schema_name, table_name, shape_hash,
		method, key_cols, ddl, improvement_pct, min_improvement_pct, measure_count, workload)
		VALUES ($1, 't', $2, 'btree', $3, 'CREATE INDEX i ON t (x)', 0, 10, $4, $5::jsonb)`
	hash := strings.Repeat("ab", 32)
	if _, err := pool.Exec(ctx, insert, tag, hash, []string{"x"}, 1, "[]"); err != nil {
		t.Fatalf("valid row refused: %v", err)
	}
	var db string
	if err := pool.QueryRow(ctx, `SELECT database_name FROM sage.optimizer_rejection
		WHERE schema_name = $1`, tag).Scan(&db); err != nil || db == "" {
		t.Fatalf("database_name default = %q (%v), want current_database()", db, err)
	}
	bad := []struct {
		name     string
		hash     string
		keys     []string
		count    int
		workload string
	}{
		{"duplicate shape", hash, []string{"x"}, 1, "[]"},
		{"short hash", "abc", []string{"x"}, 1, "[]"},
		{"no keys", strings.Repeat("cd", 32), []string{}, 1, "[]"},
		{"zero count", strings.Repeat("ef", 32), []string{"x"}, 0, "[]"},
		{"object workload", strings.Repeat("01", 32), []string{"x"}, 1, "{}"},
	}
	for _, b := range bad {
		if _, err := pool.Exec(ctx, insert, tag, b.hash, b.keys, b.count, b.workload); err == nil {
			t.Errorf("%s was accepted", b.name)
		}
	}
}
