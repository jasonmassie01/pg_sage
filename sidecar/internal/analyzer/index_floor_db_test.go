package analyzer

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Integration (indexes-12): an analyzer with no tuning agent (llm off)
// and the index floor proposes one HypoPG-verified missing_index for a
// seq-scan workload on orders(customer_id), persists it once across
// cycles, and never re-proposes a shape the agent left open.

func floorPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "analyzer_floor"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	setup := `CREATE TABLE public.orders (id bigserial PRIMARY KEY, customer_id int NOT NULL,
			total numeric NOT NULL);
		INSERT INTO public.orders (customer_id, total)
			SELECT g % 5000, g FROM generate_series(1, 50000) g;
		ANALYZE public.orders`
	if _, err := pool.Exec(ctx, setup); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, "CREATE EXTENSION hypopg"); err != nil {
		t.Skipf("HypoPG server extension unavailable on this image: %v", err)
	}
	return pool
}

func floorCycleSnap() *collector.Snapshot {
	return &collector.Snapshot{
		Tables: []collector.TableStats{{SchemaName: "public", RelName: "orders",
			NLiveTup: 50000, SeqScan: 900}},
		Indexes: []collector.IndexStats{{SchemaName: "public", RelName: "orders",
			IndexRelName: "orders_pkey", IsUnique: true, IsPrimary: true, IsValid: true,
			IndexDef: "CREATE UNIQUE INDEX orders_pkey ON public.orders USING btree (id)"}},
		Queries: []collector.QueryStats{{QueryID: 11, Calls: 900, MeanExecTime: 6,
			TotalExecTime: 5400, Query: "SELECT id, total FROM orders WHERE customer_id = $1"}},
	}
}

func floorCycleAnalyzer(pool *pgxpool.Pool) *Analyzer {
	cfg := config.DefaultConfig()
	cfg.LLM.Optimizer.MinSnapshots = 0
	cfg.LLM.Optimizer.MinQueryCalls = 1
	logFn := func(string, string, ...any) {}
	a := New(pool, cfg, nil, nil, nil, nil, nil, logFn)
	a.WithIndexFloor(optimizer.NewFloor(optimizer.New(pool, &cfg.LLM.Optimizer, logFn),
		logFn))
	return a
}

func floorCycle(t *testing.T, a *Analyzer) []Finding {
	t.Helper()
	a.eval = newCycleEval()
	a.eval.seqHeavy = []string{"public.orders"}
	return a.runProducers(context.Background(), floorCycleSnap(), nil)
}

func openRows(t *testing.T, pool *pgxpool.Pool, ident string) (int, string) {
	t.Helper()
	var n int
	var src string
	err := pool.QueryRow(context.Background(), `SELECT count(*),
		COALESCE(max(detail->>'source'), '') FROM sage.findings
		WHERE category = 'missing_index' AND object_identifier = $1 AND status = 'open'`,
		ident).Scan(&n, &src)
	if err != nil {
		t.Fatalf("read findings: %v", err)
	}
	return n, src
}

func TestIndexFloorDB_NoLLMRuntimeProposesTheVerifiedIndexOnce(t *testing.T) {
	pool := floorPool(t)
	a := floorCycleAnalyzer(pool)
	ctx := context.Background()
	for cycle := 1; cycle <= 2; cycle++ {
		out := floorCycle(t, a)
		if len(out) != 1 || out[0].ObjectIdentifier != customerID {
			t.Fatalf("cycle %d: findings = %+v", cycle, out)
		}
		f := out[0]
		if f.Detail["source"] != optimizer.FloorSource ||
			f.Detail["what_if_verdict"] != optimizer.WhatIfVerified ||
			f.RecommendedSQL == "" || f.RollbackSQL == "" {
			t.Fatalf("cycle %d: finding = %+v", cycle, f)
		}
		if err := UpsertFindings(ctx, pool, out); err != nil {
			t.Fatalf("cycle %d: upsert: %v", cycle, err)
		}
		if n, src := openRows(t, pool, customerID); n != 1 || src != optimizer.FloorSource {
			t.Fatalf("cycle %d: open rows %d source %q", cycle, n, src)
		}
	}
	if !a.eval.resolvable(nil)["missing_index"] {
		t.Fatal("a complete floor run judges its own findings")
	}
}

func TestIndexFloorDB_AgentShapeIsNotProposedAgain(t *testing.T) {
	pool := floorPool(t)
	agent := Finding{Category: "missing_index", Severity: "info", ObjectType: "index",
		ObjectIdentifier: customerID, Title: "Index recommendation for public.orders",
		Detail:         map[string]any{"producer": "tuning_agent", "table": "public.orders"},
		Recommendation: "the agent's", RecommendedSQL: "CREATE INDEX CONCURRENTLY " +
			"orders_customer_idx ON public.orders (customer_id)"}
	if err := UpsertFindings(context.Background(), pool, []Finding{agent}); err != nil {
		t.Fatalf("seed the agent finding: %v", err)
	}
	a := floorCycleAnalyzer(pool)
	if out := floorCycle(t, a); len(out) != 0 {
		t.Fatalf("the floor re-proposed the agent's open shape: %+v", out)
	}
	if !a.eval.kept("missing_index")[customerID] {
		t.Fatal("the agent's open finding must not resolve on a floor cycle")
	}
	if n, src := openRows(t, pool, customerID); n != 1 || src != "" {
		t.Fatalf("open rows %d source %q, want the agent's alone", n, src)
	}
}
