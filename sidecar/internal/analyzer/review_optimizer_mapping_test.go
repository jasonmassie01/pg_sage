package analyzer

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/optimizer"
)

func mappingRec(ddl, category string) optimizer.Recommendation {
	return optimizer.Recommendation{
		Table: "public.orders", DDL: ddl, Category: category,
		Severity: "warning",
	}
}

// C05/G2-B19/G3-B13: two index candidates for one table persist as two
// findings; identity is table + normalized index definition and the
// category is the fixed optimizer category, not the LLM's label.
func TestOptimizerFinding_IdentityPerIndexDefinition(t *testing.T) {
	a := OptimizerRecommendationFinding(mappingRec(
		"CREATE INDEX CONCURRENTLY idx_a ON public.orders (status)", "missing_index"), "")
	b := OptimizerRecommendationFinding(mappingRec(
		"CREATE INDEX CONCURRENTLY idx_b ON public.orders (created_at) INCLUDE (id)",
		"covering_index"), "")
	if a.ObjectIdentifier == b.ObjectIdentifier {
		t.Fatalf("two candidates share identity %q", a.ObjectIdentifier)
	}
	if a.Category != "missing_index" || b.Category != "missing_index" {
		t.Errorf("categories = %q/%q, want missing_index", a.Category, b.Category)
	}
	if b.Detail["index_category"] != "covering_index" {
		t.Errorf("LLM category not preserved in detail: %v", b.Detail["index_category"])
	}
	for _, f := range []Finding{a, b} {
		if got := OptimizerFindingTable(f); got != "public.orders" {
			t.Errorf("OptimizerFindingTable(%q) = %q, want public.orders",
				f.ObjectIdentifier, got)
		}
	}
	kept := DeduplicateFindings([]Finding{a, b}, 0, func(string, string, ...any) {})
	if len(kept) != 2 {
		t.Errorf("dedup kept %d, want 2", len(kept))
	}
}

// Renaming the index or reformatting the DDL keeps the same identity so
// the open finding is updated, not duplicated.
func TestOptimizerFinding_IdentityStableAcrossCosmeticChanges(t *testing.T) {
	a := OptimizerRecommendationFinding(mappingRec(
		"CREATE INDEX CONCURRENTLY idx_a ON public.orders (status)", "missing_index"), "")
	b := OptimizerRecommendationFinding(mappingRec(
		"create index concurrently IDX_other on public.orders using BTREE ( Status );",
		"partial_index"), "")
	if a.ObjectIdentifier != b.ObjectIdentifier {
		t.Errorf("identity changed: %q vs %q", a.ObjectIdentifier, b.ObjectIdentifier)
	}
}

// Legacy rows used the bare table as identity; the table helper keeps
// working for them.
func TestOptimizerFindingTable_LegacyIdentity(t *testing.T) {
	f := Finding{ObjectIdentifier: "public.orders"}
	if got := OptimizerFindingTable(f); got != "public.orders" {
		t.Errorf("OptimizerFindingTable = %q", got)
	}
}
