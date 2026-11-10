package probes

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// vacuum-08: the freeze custodian's horizon reader on real PostgreSQL.
// No concurrency test here: the reader is a stateless query; the sampler
// that shares it is tested in internal/autonomy.

// freezeFixturePool connects to a disposable database of its own, so a
// large catalog never reaches the other tests of the package.
func freezeFixturePool(t *testing.T, label string) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.CreateDatabase(t, label)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to %s: %v", label, err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

func fixtureExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql); err != nil {
		t.Fatalf("%s: %v", strings.SplitN(sql, "\n", 2)[0], err)
	}
}

// Footprint: on 10,000 tables the reader ranks from pg_class alone and
// calls the statistics functions only for the rows it returns.
func TestFreezeHorizons_StatisticsOnlyForReturnedRowsOn10kTables(t *testing.T) {
	pool, ctx := freezeFixturePool(t, "freeze_footprint")
	// COMMIT every 500 tables: one transaction would need a lock per table.
	fixtureExec(t, ctx, pool, `CREATE SCHEMA fp; CREATE PROCEDURE fp.make() LANGUAGE plpgsql AS $$
		BEGIN FOR i IN 1..10000 LOOP
			EXECUTE format('CREATE TABLE fp.t%s (id int, note text)', i);
			IF i % 500 = 0 THEN COMMIT; END IF;
		END LOOP; END $$`)
	fixtureExec(t, ctx, pool, "CALL fp.make()")
	const limit = 50
	plan, err := testdb.Explain(ctx, pool, "ANALYZE, VERBOSE", FreezeHorizonsSQL,
		limit, int64(1<<30))
	if err != nil {
		t.Fatalf("explain the reader: %v", err)
	}
	statsNodes := 0
	plan.Walk(func(n testdb.PlanNode) {
		if n.Relation == "pg_stat_all_tables" || n.Relation == "pg_stat_user_tables" {
			t.Errorf("reader scans %s", n.Relation)
		}
		for _, out := range n.Output {
			if strings.Contains(out, "pg_stat_get_") {
				statsNodes++
				if rows := n.ActualRows * n.ActualLoops; rows > limit {
					t.Errorf("%s computes statistics for %v rows, want at most %d",
						n.NodeType, rows, limit)
				}
				return
			}
		}
	})
	if statsNodes == 0 {
		t.Fatalf("no plan node computes the statistics; the plan shape is unexpected")
	}
	// A table and its TOAST both ranked come back once: up to limit tables.
	horizons, err := ReadFreezeHorizons(ctx, pool, limit, 1<<30)
	if err != nil || len(horizons) < limit/2 || len(horizons) > limit {
		t.Fatalf("horizons = %d (%v), want between %d and %d", len(horizons), err,
			limit/2, limit)
	}
	seen := map[int64]bool{}
	for _, h := range horizons {
		if seen[h.OID] || h.Schema == "pg_toast" {
			t.Fatalf("table %s.%s returned twice or as a TOAST relation", h.Schema, h.Table)
		}
		seen[h.OID] = true
	}
}

// A table aged through its TOAST relation is returned as the table, once.
func TestFreezeHorizons_TOASTHorizonIsItsOwner(t *testing.T) {
	pool, ctx := freezeFixturePool(t, "freeze_toast")
	fixtureExec(t, ctx, pool, `CREATE TABLE public.docs (id int, body text)
		WITH (autovacuum_enabled = false, toast.autovacuum_enabled = false,
		      toast.autovacuum_freeze_max_age = 100000);
		INSERT INTO public.docs SELECT 1, string_agg(md5(g::text), '')
		FROM generate_series(1, 2000) g`)
	fixtureExec(t, ctx, pool, "VACUUM (FREEZE) public.docs")
	testdb.BurnXIDs(t, ctx, pool, 3000)
	fixtureExec(t, ctx, pool, "VACUUM (FREEZE, PROCESS_TOAST false) public.docs")
	horizons, err := ReadFreezeHorizons(ctx, pool, 50, 0)
	if err != nil {
		t.Fatalf("ReadFreezeHorizons: %v", err)
	}
	var found []FreezeHorizon
	for _, h := range horizons {
		if h.Schema == "pg_toast" {
			t.Fatalf("a TOAST relation was returned as itself: %+v", h)
		}
		if h.Schema == "public" && h.Table == "docs" {
			found = append(found, h)
		}
	}
	if len(found) != 1 {
		t.Fatalf("public.docs returned %d times in %+v, want once", len(found), horizons)
	}
	h := found[0]
	if !h.ViaTOAST || h.XIDAge < 3000 || h.XIDMaxAge != 100000 || h.Catalog {
		t.Fatalf("docs horizon = %+v, want its TOAST's age (>= 3000) and maximum 100000", h)
	}
	if h.SettingXIDMaxAge <= h.XIDMaxAge {
		t.Fatalf("setting maximum %d must stay the server's, not the reloption %d",
			h.SettingXIDMaxAge, h.XIDMaxAge)
	}
}

// Partitioned parents have no relfrozenxid and are never returned.
func TestFreezeHorizons_SkipPartitionedParents(t *testing.T) {
	pool, ctx := freezeFixturePool(t, "freeze_parts")
	fixtureExec(t, ctx, pool, `CREATE TABLE public.ev (id int) PARTITION BY RANGE (id);
		CREATE TABLE public.ev_1 PARTITION OF public.ev FOR VALUES FROM (0) TO (10)`)
	horizons, err := ReadFreezeHorizons(ctx, pool, 500, 0)
	if err != nil {
		t.Fatalf("ReadFreezeHorizons: %v", err)
	}
	leaf := false
	for _, h := range horizons {
		if h.Schema == "public" && h.Table == "ev" {
			t.Fatalf("partitioned parent returned: %+v", h)
		}
		leaf = leaf || (h.Schema == "public" && h.Table == "ev_1")
	}
	if !leaf {
		t.Fatalf("leaf partition public.ev_1 missing from %d horizons", len(horizons))
	}
}
