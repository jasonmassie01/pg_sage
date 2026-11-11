package optimizer

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Integration (indexes-12): a no-LLM runtime with a seq-scan workload on
// orders(customer_id) gets one missing_index candidate for (customer_id),
// HypoPG-measured where HypoPG is installed and unmeasured where it is not.

// floorDatabase is a fresh database holding public.orders (50k rows, 5000
// customers), analyzed; withHypoPG installs the extension.
func floorDatabase(t *testing.T, withHypoPG bool) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), testdb.CreateDatabase(t, "index_floor"))
	if err != nil {
		t.Fatalf("connect to the floor database: %v", err)
	}
	t.Cleanup(pool.Close)
	setup := `CREATE TABLE public.orders (id bigserial PRIMARY KEY, customer_id int NOT NULL,
			status text NOT NULL, total numeric NOT NULL);
		INSERT INTO public.orders (customer_id, status, total)
			SELECT g % 5000, CASE WHEN g % 10 = 0 THEN 'open' ELSE 'done' END, g
			FROM generate_series(1, 50000) g;
		ANALYZE public.orders`
	if _, err := pool.Exec(t.Context(), setup); err != nil {
		t.Fatalf("floor fixture: %v", err)
	}
	if withHypoPG {
		if _, err := pool.Exec(t.Context(), "CREATE EXTENSION hypopg"); err != nil {
			t.Skipf("HypoPG server extension unavailable on this image, the measured "+
				"floor is not verified: %v", err)
		}
	}
	return pool
}

func floorSnapshot() *collector.Snapshot {
	return &collector.Snapshot{
		Tables: []collector.TableStats{{SchemaName: "public", RelName: "orders",
			NLiveTup: 50000, SeqScan: 900, SeqTupRead: 45_000_000}},
		Indexes: []collector.IndexStats{{SchemaName: "public", RelName: "orders",
			IndexRelName: "orders_pkey", IsUnique: true, IsPrimary: true, IsValid: true,
			IndexDef: "CREATE UNIQUE INDEX orders_pkey ON public.orders USING btree (id)"}},
		Queries: []collector.QueryStats{
			{QueryID: 11, Calls: 900, MeanExecTime: 6, TotalExecTime: 5400,
				Query: "SELECT id, total FROM orders WHERE customer_id = $1"},
			{QueryID: 12, Calls: 40, MeanExecTime: 9, TotalExecTime: 360,
				Query: "SELECT count(*) FROM orders WHERE status = $1"},
		},
	}
}

func floorOptimizer(pool *pgxpool.Pool) *Optimizer {
	return New(pool, &config.OptimizerConfig{Enabled: true, MinQueryCalls: 1,
		MaxIndexesPerTable: 10, MaxNewPerTable: 3, HypoPGMinImprovePct: 10,
		WriteHeavyRatioPct: 70}, noopLog2)
}

func TestFloor_HypoPGAdmitsTheCustomerIndex(t *testing.T) {
	pool := floorDatabase(t, true)
	floor := NewFloor(floorOptimizer(pool), noopLog2)
	res := floor.Run(context.Background(), floorSnapshot(), []string{"public.orders"}, nil)
	if !res.Ran || len(res.Incomplete) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Admitted) != 1 {
		t.Fatalf("admitted %d candidates, want exactly one: %+v", len(res.Admitted),
			res.Admitted)
	}
	rec := res.Admitted[0].Rec
	if id := rec.FindingIdentifier(); id != "public.orders|btree(customer_id)" {
		t.Fatalf("identifier = %q", id)
	}
	if rec.WhatIf != WhatIfVerified || !rec.Validated || rec.EstimatedImprovementPct <= 0 {
		t.Fatalf("HypoPG must measure the candidate: verdict %q (%s), improvement %.1f",
			rec.WhatIf, rec.WhatIfReason, rec.EstimatedImprovementPct)
	}
	if rec.DropDDL == "" || len(rec.AffectedQueryIDs) == 0 {
		t.Fatalf("admission fills the rollback and the queries it helps: %+v", rec)
	}
	var hypo int
	if err := pool.QueryRow(t.Context(),
		"SELECT count(*) FROM pg_indexes WHERE tablename = 'orders'").Scan(&hypo); err != nil {
		t.Fatalf("count indexes: %v", err)
	}
	if hypo != 1 {
		t.Fatalf("the floor created a real index: %d indexes on orders", hypo)
	}
}

func TestFloor_WithoutHypoPGTheCandidateIsUnmeasured(t *testing.T) {
	pool := floorDatabase(t, false)
	floor := NewFloor(floorOptimizer(pool), noopLog2)
	res := floor.Run(context.Background(), floorSnapshot(), []string{"public.orders"}, nil)
	if len(res.Admitted) != 1 {
		t.Fatalf("admitted = %+v", res.Admitted)
	}
	rec := res.Admitted[0].Rec
	if rec.FindingIdentifier() != "public.orders|btree(customer_id)" ||
		rec.WhatIf != WhatIfUnverified || rec.WhatIfReason != "HypoPG unavailable" ||
		rec.Validated {
		t.Fatalf("rec = %+v", rec)
	}
}

// A second cycle with the first finding open proposes nothing new.
func TestFloor_OpenShapeIsNotProposedAgain(t *testing.T) {
	pool := floorDatabase(t, false)
	floor := NewFloor(floorOptimizer(pool), noopLog2)
	first := floor.Run(context.Background(), floorSnapshot(), []string{"public.orders"}, nil)
	if len(first.Admitted) != 1 {
		t.Fatalf("first cycle admitted %+v", first.Admitted)
	}
	open := map[string]bool{first.Admitted[0].Rec.FindingIdentifier(): true}
	second := floor.Run(context.Background(), floorSnapshot(), []string{"public.orders"}, open)
	if len(second.Admitted) != 0 || !second.Ran {
		t.Fatalf("second cycle = %+v", second)
	}
}
