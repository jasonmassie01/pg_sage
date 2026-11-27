package provideradvice

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// The what-if reads the table's workload from pg_stat_statements in the
// schema the extension is installed in, not through the search_path
// (security-01c), and an install without the extension is an empty
// workload, not an error.
// No concurrent access tests: whatIfInputs holds no shared state; each call
// runs in its own read-only transaction.

// whatIfDB is a fresh database whose pool's search_path is public only;
// pssSchema names the schema pg_stat_statements is created in ("" for none).
func whatIfDB(t *testing.T, ctx context.Context, name, pssSchema string) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.CreateDatabase(t, name)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)
	stmts := []string{"DROP EXTENSION IF EXISTS pg_stat_statements",
		"CREATE TABLE public.pa_wl_orders (id int PRIMARY KEY, customer int)"}
	if pssSchema != "" {
		stmts = append(stmts, `CREATE SCHEMA "`+pssSchema+`"`,
			`CREATE EXTENSION pg_stat_statements SCHEMA "`+pssSchema+`"`)
	}
	for _, stmt := range stmts {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect with search_path public: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestWhatIfInputs_ReadsWorkloadOutsideTheSearchPath(t *testing.T) {
	testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool := whatIfDB(t, ctx, "pawhatifext", "ext ws")
	const probe = "SELECT count(*) FROM public.pa_wl_orders WHERE customer = 42"
	if _, err := pool.Exec(ctx, probe); err != nil {
		t.Fatalf("seed a statement: %v", err)
	}
	ev := NewDBEvaluator(pool, nil)
	ddl := "CREATE INDEX ON public.pa_wl_orders (customer)"
	queries, served, err := ev.whatIfInputs(ctx, ddl, "public.pa_wl_orders", "pa_wl_orders")
	if err != nil || served != "" {
		t.Fatalf("whatIfInputs: served %q, err %v", served, err)
	}
	for _, q := range queries {
		if strings.Contains(q.Text, "pa_wl_orders WHERE customer = $1") && q.Calls >= 1 {
			return
		}
	}
	t.Fatalf("workload %+v lacks the seeded statement on pa_wl_orders", queries)
}

func TestWhatIfInputs_NoExtensionIsAnEmptyWorkload(t *testing.T) {
	testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool := whatIfDB(t, ctx, "pawhatifnone", "")
	var logged []string
	ev := NewDBEvaluator(pool, func(_, format string, args ...any) {
		logged = append(logged, format)
	})
	ddl := "CREATE INDEX ON public.pa_wl_orders (customer)"
	queries, served, err := ev.whatIfInputs(ctx, ddl, "public.pa_wl_orders", "pa_wl_orders")
	if err != nil || served != "" || len(queries) != 0 {
		t.Fatalf("whatIfInputs = %+v, served %q, err %v; want an empty workload", queries,
			served, err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "not installed") {
		t.Fatalf("logged %q, want one note that pg_stat_statements is not installed", logged)
	}
	act, reason := ev.WhatIf(ctx, ddl)
	if act || !strings.HasPrefix(reason, "would not act: ") {
		t.Fatalf("WhatIf without pg_stat_statements: act %v, %q", act, reason)
	}
}
