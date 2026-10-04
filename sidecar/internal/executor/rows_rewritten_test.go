package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
)

// rewriteTarget names the relation a statement rewrites, from the SQL
// alone: VACUUM FULL, CLUSTER, non-concurrent REINDEX and the ALTER TABLE
// forms that rewrite the heap. Everything else rewrites nothing.
func TestRewriteTargetClassifiesRewritingStatements(t *testing.T) {
	tests := []struct {
		sql      string
		relation string
		rewrites bool
	}{
		{"VACUUM FULL public.t", "public.t", true},
		{"vacuum full public.t;", "public.t", true},
		{"VACUUM (FULL, ANALYZE) public.t", "public.t", true},
		{"VACUUM (VERBOSE, FULL) public.t", "public.t", true},
		{`VACUUM FULL "Public"."T"`, `"Public"."T"`, true},
		{"VACUUM FULL", "", true},
		{"VACUUM (ANALYZE) public.t", "", false},
		{"VACUUM public.t", "", false},
		{"VACUUM (FULL false) public.t", "", false},
		{"CLUSTER public.t USING t_pkey", "public.t", true},
		{"CLUSTER VERBOSE public.t", "public.t", true},
		{"CLUSTER", "", true},
		{"REINDEX TABLE public.t", "public.t", true},
		{"REINDEX INDEX public.i", "public.i", true},
		{"REINDEX (VERBOSE) TABLE public.t", "public.t", true},
		{"REINDEX TABLE CONCURRENTLY public.t", "", false},
		{"REINDEX INDEX CONCURRENTLY public.i", "", false},
		{"REINDEX (CONCURRENTLY) TABLE public.t", "", false},
		{"REINDEX SCHEMA public", "", true},
		{"ALTER TABLE public.t ALTER COLUMN c TYPE bigint", "public.t", true},
		{"ALTER TABLE public.t ALTER COLUMN c SET DATA TYPE text", "public.t", true},
		{"ALTER TABLE public.t ALTER c TYPE bigint USING c::bigint", "public.t", true},
		{"ALTER TABLE IF EXISTS ONLY public.t ALTER COLUMN c TYPE int", "public.t", true},
		{"ALTER TABLE public.t SET LOGGED", "public.t", true},
		{"ALTER TABLE public.t SET UNLOGGED", "public.t", true},
		{"ALTER TABLE public.t SET TABLESPACE fast", "public.t", true},
		{"ALTER TABLE public.t SET ACCESS METHOD heap2", "public.t", true},
		{"ALTER TABLE public.t ADD COLUMN c int DEFAULT random()", "public.t", true},
		{"ALTER TABLE public.t ADD COLUMN c timestamptz DEFAULT clock_timestamp()",
			"public.t", true},
		{"ALTER TABLE public.t ADD COLUMN c uuid DEFAULT gen_random_uuid() NOT NULL",
			"public.t", true},
		{"ALTER TABLE public.t ADD c int GENERATED ALWAYS AS (a * 2) STORED",
			"public.t", true},
		{"ALTER TABLE public.t ADD COLUMN id bigint GENERATED ALWAYS AS IDENTITY",
			"public.t", true},
		{"ALTER TABLE public.t ADD COLUMN a int, ALTER COLUMN b TYPE text", "public.t", true},
		{"ALTER TABLE public.t ADD COLUMN c int DEFAULT 0", "", false},
		{"ALTER TABLE public.t ADD COLUMN c int DEFAULT 0 NOT NULL", "", false},
		{"ALTER TABLE public.t ADD COLUMN c numeric DEFAULT -1.5::numeric", "", false},
		{"ALTER TABLE public.t ADD COLUMN c text DEFAULT 'random()'", "", false},
		{"ALTER TABLE public.t ADD COLUMN c text DEFAULT 'it''s'", "", false},
		{"ALTER TABLE public.t ADD COLUMN c boolean DEFAULT false", "", false},
		{"ALTER TABLE public.t ADD COLUMN c int DEFAULT NULL", "", false},
		{"ALTER TABLE public.t ADD COLUMN c int", "", false},
		{"ALTER TABLE public.t SET (fillfactor = 70)", "", false},
		{"ALTER TABLE public.t RESET (autovacuum_vacuum_scale_factor)", "", false},
		{"ALTER TABLE public.t ALTER COLUMN c SET NOT NULL", "", false},
		{"ALTER TABLE public.t ADD CONSTRAINT c_nn CHECK (c IS NOT NULL) NOT VALID", "", false},
		{"ALTER TABLE public.t VALIDATE CONSTRAINT c_nn", "", false},
		{"CREATE INDEX CONCURRENTLY i ON public.t (a)", "", false},
		{"DROP INDEX CONCURRENTLY public.i", "", false},
		{"ANALYZE public.t", "", false},
		{"ALTER SYSTEM SET work_mem = '64MB'", "", false},
		{"SELECT pg_cancel_backend(42)", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		relation, rewrites := rewriteTarget(tt.sql)
		if relation != tt.relation || rewrites != tt.rewrites {
			t.Errorf("rewriteTarget(%q) = (%q, %v), want (%q, %v)",
				tt.sql, relation, rewrites, tt.relation, tt.rewrites)
		}
	}
}

// estimateRowsRewritten reads the live estimate of the rewritten table at
// decision time: reltuples (or live tuples), every leaf partition of a
// partitioned table, and an index's table for REINDEX INDEX.
func TestEstimateRowsRewrittenFromCatalog(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	exec := New(pool, config.DefaultConfig(), time.Time{}, func(string, string, ...any) {})
	mustExec(t, pool, ctx, `CREATE TABLE public.rw_t (id int);
		INSERT INTO public.rw_t SELECT g FROM generate_series(1, 400) g;
		CREATE INDEX rw_t_id ON public.rw_t (id);
		CREATE TABLE public.rw_p (id int) PARTITION BY RANGE (id);
		CREATE TABLE public.rw_p1 PARTITION OF public.rw_p FOR VALUES FROM (0) TO (1000);
		CREATE TABLE public.rw_p2 PARTITION OF public.rw_p FOR VALUES FROM (1000) TO (2000);
		INSERT INTO public.rw_p SELECT g FROM generate_series(1, 100) g;
		INSERT INTO public.rw_p SELECT g FROM generate_series(1000, 1049) g;
		ANALYZE public.rw_t; ANALYZE public.rw_p1; ANALYZE public.rw_p2;`)
	tests := []struct {
		sql  string
		want int64
	}{
		{"VACUUM FULL public.rw_t", 400},
		{`VACUUM (FULL) "public"."rw_t"`, 400},
		{"CLUSTER public.rw_t USING rw_t_id", 400},
		{"REINDEX INDEX public.rw_t_id", 400},
		{"REINDEX TABLE public.rw_t", 400},
		{"ALTER TABLE public.rw_t ALTER COLUMN id TYPE bigint", 400},
		{"VACUUM FULL public.rw_p", 150},
		{"ALTER TABLE public.rw_t ADD COLUMN c int DEFAULT 0", 0},
		{"VACUUM public.rw_t", 0},
		{"REINDEX INDEX CONCURRENTLY public.rw_t_id", 0},
		{"CREATE INDEX CONCURRENTLY rw_x ON public.rw_t (id)", 0},
		{"", 0},
	}
	for _, tt := range tests {
		got, err := exec.estimateRowsRewritten(ctx, tt.sql)
		if err != nil || got != tt.want {
			t.Errorf("estimateRowsRewritten(%q) = %d, %v; want %d", tt.sql, got, err, tt.want)
		}
	}
}

// A rewrite whose table cannot be resolved cannot be bounded, so it is an
// error (the gate then blocks it), never an estimate of zero.
func TestEstimateRowsRewrittenFailsClosed(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	exec := New(pool, config.DefaultConfig(), time.Time{}, func(string, string, ...any) {})
	for _, sql := range []string{
		"VACUUM FULL public.no_such_table", "VACUUM FULL", "CLUSTER",
		"REINDEX SCHEMA public",
	} {
		got, err := exec.estimateRowsRewritten(ctx, sql)
		if err == nil || !strings.Contains(err.Error(), "rows rewritten") {
			t.Errorf("estimateRowsRewritten(%q) = %d, %v; want a rows-rewritten error",
				sql, got, err)
		}
	}
	none := New(nil, config.DefaultConfig(), time.Time{}, func(string, string, ...any) {})
	if _, err := none.estimateRowsRewritten(context.Background(),
		"VACUUM FULL public.t"); err == nil {
		t.Fatal("estimate without a pool: want an error")
	}
	if got, err := none.estimateRowsRewritten(context.Background(),
		"ANALYZE public.t"); err != nil || got != 0 {
		t.Fatalf("non-rewrite without a pool = %d, %v; want 0, nil (no catalog read)", got, err)
	}
}

func mustExec(t *testing.T, pool *pgxpool.Pool, ctx context.Context, sql string) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql); err != nil {
		t.Fatalf("exec: %v\n%s", err, sql)
	}
}
