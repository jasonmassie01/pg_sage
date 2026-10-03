package optimizer

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Releases before v1.8.0 stored LLM index advice under the LLM's own label
// (covering_index, partial_index, composite_index, …) with the bare table
// as identity, an LLM-authored drop_ddl and no what-if verdict. Such a
// finding is reloaded like a missing_index one — canonicalized (derived
// rollback), validated and re-verified — re-emitted under the current
// identity, and the legacy row is retired so the old copy never acts.

func TestIsOptimizerFinding(t *testing.T) {
	cases := []struct {
		category string
		detail   map[string]any
		want     bool
	}{
		{"missing_index", nil, true},
		{"covering_index", nil, true},
		{"partial_index", map[string]any{}, true},
		{"composite_index", nil, true},
		{"expression_index", map[string]any{"llm_rationale": "x"}, true},
		{"gin_index", map[string]any{"plan_source": "query_text_only"}, true},
		{"whatever", map[string]any{"hypopg_validated": false}, true},
		{"whatever", map[string]any{"what_if_verdict": "unverified"}, true},
		{"missing_fk_index", map[string]any{"constraint": "fk"}, false},
		{"missing_fk_index", nil, false},
		{"unused_index", map[string]any{"idx_scan": 0}, false},
		{"", nil, false},
	}
	for _, c := range cases {
		if got := IsOptimizerFinding(c.category, c.detail); got != c.want {
			t.Errorf("IsOptimizerFinding(%q, %v) = %t, want %t", c.category, c.detail,
				got, c.want)
		}
	}
	cats := Categories()
	if len(cats) != 4 || cats[0] != OptimizerCategory {
		t.Fatalf("Categories() = %v, want missing_index first plus 3 legacy labels", cats)
	}
}

func legacyRowDetail(category, ddl string) map[string]any {
	return map[string]any{"ddl": ddl, "category": category, "index_type": "btree",
		"drop_ddl":      "DROP INDEX CONCURRENTLY IF EXISTS idx_orders_old_name",
		"llm_rationale": "covers the hot lookup", "plan_source": "query_text_only",
		"hypopg_validated": false, "confidence_score": 0.8, "queryids": []int64{7}}
}

func insertLegacyFinding(t *testing.T, pool *pgxpool.Pool, category string,
	detail map[string]any, acted bool) int64 {
	t.Helper()
	insertOpenFinding(t, pool, category, "public.orders", detail)
	var id int64
	if err := pool.QueryRow(context.Background(), `UPDATE sage.findings
		SET acted_on_at = CASE WHEN $2 THEN now() END
		WHERE category = $1 AND object_identifier = 'public.orders' AND status = 'open'
		RETURNING id`, category, acted).Scan(&id); err != nil {
		t.Fatalf("mark finding: %v", err)
	}
	return id
}

func findingStatus(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM sage.findings WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func legacyOptimizer(pool *pgxpool.Pool) *Optimizer {
	o := New(nil, nil, pool, fnTestOptimizerConfig(), 160000, 8192, fnNoopLog)
	unavailable := false
	o.hypopg.available = &unavailable
	return o
}

func TestOpenRecommendations_ReloadsAndRetiresLegacyCategories(t *testing.T) {
	for _, category := range []string{"covering_index", "partial_index", "composite_index"} {
		t.Run(category, func(t *testing.T) {
			pool := connectFindingsDB(t)
			ddl := "CREATE INDEX CONCURRENTLY idx_orders_status ON public.orders (status)"
			id := insertLegacyFinding(t, pool, category, legacyRowDetail(category, ddl), false)
			recs, open := legacyOptimizer(pool).openRecommendations(context.Background(),
				sampleTableContext())
			if !open || len(recs) != 1 {
				t.Fatalf("open=%t recs=%d, want the legacy candidate re-emitted", open,
					len(recs))
			}
			r := recs[0]
			if r.Category != OptimizerCategory || r.IndexCategory != category ||
				r.FindingIdentifier() != "public.orders|btree(status)" {
				t.Fatalf("re-emitted %q/%q as %q, want missing_index with label %q",
					r.Category, r.IndexCategory, r.FindingIdentifier(), category)
			}
			if r.DropDDL != `DROP INDEX CONCURRENTLY IF EXISTS "public"."idx_orders_status"` {
				t.Fatalf("rollback %q: the LLM's stale drop_ddl was kept", r.DropDDL)
			}
			if r.WhatIf != WhatIfUnverified || r.Validated {
				t.Fatalf("verdict %q validated=%t, want unverified without HypoPG",
					r.WhatIf, r.Validated)
			}
			if got := findingStatus(t, pool, id); got != "resolved" {
				t.Fatalf("legacy finding status = %q, want resolved after re-emission", got)
			}
		})
	}
}

// A legacy candidate that no longer passes the deterministic gates (here:
// it targets another table) is not re-emitted, and is retired too.
func TestOpenRecommendations_RetiresInvalidLegacyCandidate(t *testing.T) {
	pool := connectFindingsDB(t)
	ddl := "CREATE INDEX CONCURRENTLY idx_x ON public.customers (status)"
	id := insertLegacyFinding(t, pool, "covering_index",
		legacyRowDetail("covering_index", ddl), false)
	recs, open := legacyOptimizer(pool).openRecommendations(context.Background(),
		sampleTableContext())
	if !open || len(recs) != 0 {
		t.Fatalf("open=%t recs=%+v, want nothing re-emitted", open, recs)
	}
	if got := findingStatus(t, pool, id); got != "resolved" {
		t.Fatalf("invalid legacy finding status = %q, want resolved", got)
	}
}

// A legacy row an action is already acting on is neither reloaded nor
// retired; it still counts as open (no new LLM call for the table).
func TestOpenRecommendations_KeepsActedOnLegacyFinding(t *testing.T) {
	pool := connectFindingsDB(t)
	ddl := "CREATE INDEX CONCURRENTLY idx_orders_status ON public.orders (status)"
	id := insertLegacyFinding(t, pool, "partial_index",
		legacyRowDetail("partial_index", ddl), true)
	recs, open := legacyOptimizer(pool).openRecommendations(context.Background(),
		sampleTableContext())
	if !open || len(recs) != 0 {
		t.Fatalf("open=%t recs=%d, want open with nothing re-emitted", open, len(recs))
	}
	if got := findingStatus(t, pool, id); got != "open" {
		t.Fatalf("acted-on legacy finding status = %q, want open", got)
	}
}

// A current missing_index finding is reloaded but never retired by the
// optimizer: the analyzer resolves it when it is no longer emitted.
func TestOpenRecommendations_DoesNotRetireCurrentCategory(t *testing.T) {
	pool := connectFindingsDB(t)
	ddl := "CREATE INDEX CONCURRENTLY idx_orders_status ON public.orders (status)"
	insertOpenFinding(t, pool, OptimizerCategory, "public.orders|btree(status)",
		map[string]any{"ddl": ddl, "table": "public.orders"})
	if _, open := legacyOptimizer(pool).openRecommendations(context.Background(),
		sampleTableContext()); !open {
		t.Fatal("current finding not detected")
	}
	var status string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM sage.findings
		WHERE category = $1`, OptimizerCategory).Scan(&status); err != nil || status != "open" {
		t.Fatalf("current finding status = %q (%v), want open", status, err)
	}
}
