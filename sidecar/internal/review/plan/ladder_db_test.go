package plan

import (
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/optimizer"
)

// The HypoPG tier on a real server: an index the statements need predicts
// an improvement; other kinds, and a database without HypoPG, have no
// tier; a statement on a missing table is a ladder error.
func TestHypoPGLadderDB_PredictsIndexesOnly(t *testing.T) {
	pool, ctx := livePool(t)
	for _, sql := range []string{`DROP TABLE IF EXISTS public.ladder_orders`,
		`CREATE TABLE public.ladder_orders (id bigint PRIMARY KEY, region text, pad text)`,
		`INSERT INTO public.ladder_orders SELECT g, 'r' || (g % 500), repeat('x', 200)
		   FROM generate_series(1, 40000) g`,
		`ANALYZE public.ladder_orders`, `CREATE EXTENSION IF NOT EXISTS hypopg`} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	queries := []optimizer.QueryInfo{{QueryID: 1, Calls: 1000, TotalTimeMs: 5000,
		MeanTimeMs: 5, Text: "SELECT id FROM public.ladder_orders WHERE region = 'r7'"}}
	l := NewHypoPGLadder(pool, nil)
	m, err := l.Predict(ctx, Proposal{Kind: "create_index_concurrently",
		ActionType: "create_index_concurrently", Table: "public.ladder_orders",
		SQL: "CREATE INDEX CONCURRENTLY sage_ladder_orders_region_idx ON " +
			"public.ladder_orders (region)",
		Queries: queries})
	if err != nil || m.Tier != TierHypoPG || m.ImprovementPct <= 10 {
		t.Fatalf("index prediction %+v %v, want a hypopg improvement above 10%%", m, err)
	}
	_, err = l.Predict(ctx, Proposal{Kind: "create_statistics", ActionType: "create_statistics"})
	if !errors.Is(err, ErrNoTier) {
		t.Fatalf("statistics: %v, want ErrNoTier", err)
	}
	_, err = l.Predict(ctx, Proposal{Kind: "create_index_concurrently",
		ActionType: "create_index_concurrently", Table: "public.nosuch",
		SQL: "CREATE INDEX CONCURRENTLY x ON public.nosuch (a)",
		Queries: []optimizer.QueryInfo{{QueryID: 2, Calls: 10,
			Text: "SELECT 1 FROM public.nosuch"}}})
	if err == nil || errors.Is(err, ErrNoTier) || !strings.Contains(err.Error(), "hypopg") {
		t.Fatalf("missing table: %v, want a hypopg ladder error", err)
	}
	if _, err := pool.Exec(ctx, `DROP EXTENSION hypopg`); err != nil {
		t.Fatal(err)
	}
	_, err = NewHypoPGLadder(pool, nil).Predict(ctx, Proposal{Kind: "create_index_concurrently",
		ActionType: "create_index_concurrently", Table: "public.ladder_orders",
		SQL: "CREATE INDEX CONCURRENTLY y ON public.ladder_orders (region)", Queries: queries})
	if !errors.Is(err, ErrNoTier) || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("without hypopg: %v, want ErrNoTier naming the missing extension", err)
	}
}
