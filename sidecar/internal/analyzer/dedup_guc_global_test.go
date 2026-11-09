package analyzer

import (
	"strings"
	"testing"
)

// advisors-02 (product decision changed): a per-table setting no longer
// hides the global recommendation of the same GUC. Both are kept; the
// global one names the tables that keep their own setting.
func TestDedupFindings_PerTableAndGlobalGUC_BothKept(t *testing.T) {
	in := []Finding{
		{
			Category:         "memory_tuning",
			Severity:         "warning",
			ObjectIdentifier: "instance:autovacuum_vacuum_scale_factor",
			Title:            "global scale factor",
			Recommendation:   "lower the global scale factor",
			Detail:           map[string]any{"why": "write-heavy"},
			RecommendedSQL: "ALTER SYSTEM SET " +
				"autovacuum_vacuum_scale_factor = 0.05",
		},
		{
			Category:         "vacuum_tuning",
			Severity:         "warning",
			ObjectIdentifier: "public.t1",
			Title:            "per-table scale factor",
			RecommendedSQL: "ALTER TABLE public.t1 SET " +
				"(autovacuum_vacuum_scale_factor = 0.01)",
		},
	}
	original := in[0].Detail
	got := DeduplicateFindings(in, 0, noopLog)
	if len(got) != 2 {
		t.Fatalf("expected global and per-table kept, got %d: %+v", len(got), got)
	}
	global := got[0]
	if global.Title != "global scale factor" {
		t.Fatalf("got[0] = %q, want the global finding", global.Title)
	}
	tables, ok := global.Detail["overridden_by_tables"].([]string)
	if !ok || len(tables) != 1 || tables[0] != "public.t1" {
		t.Fatalf("overridden_by_tables = %#v, want [public.t1]",
			global.Detail["overridden_by_tables"])
	}
	if global.Detail["why"] != "write-heavy" {
		t.Errorf("existing detail lost: %#v", global.Detail)
	}
	if !strings.Contains(global.Recommendation, "these tables keep their own "+
		"setting; the global change does not affect them") {
		t.Errorf("rationale lacks the override line: %q", global.Recommendation)
	}
	if _, mutated := original["overridden_by_tables"]; mutated {
		t.Error("the input finding's detail map was mutated")
	}
	if got[1].Title != "per-table scale factor" {
		t.Errorf("got[1] = %q, want the per-table finding", got[1].Title)
	}
}

// Identical global recommendations are still folded into one.
func TestDedupFindings_IdenticalGlobalGUC_Folded(t *testing.T) {
	f := Finding{
		Category: "memory_tuning", Severity: "warning",
		ObjectIdentifier: "instance:autovacuum_vacuum_scale_factor",
		Title:            "global scale factor",
		RecommendedSQL:   "ALTER SYSTEM SET autovacuum_vacuum_scale_factor = 0.05",
	}
	g := f
	g.Category = "vacuum_tuning"
	got := DeduplicateFindings([]Finding{f, g}, 0, noopLog)
	if len(got) != 1 {
		t.Fatalf("expected the duplicates folded to 1, got %d", len(got))
	}
	if _, ok := got[0].Detail["overridden_by_tables"]; ok {
		t.Errorf("no per-table finding, yet overridden_by_tables = %#v", got[0].Detail)
	}
}

// Two per-table findings of the GUC on different tables both annotate the
// one global finding, sorted, once each.
func TestDedupFindings_GlobalGUC_ManyTablesSorted(t *testing.T) {
	perTable := func(table string) Finding {
		return Finding{Category: "vacuum_tuning", Severity: "info",
			ObjectIdentifier: table, Title: "pt " + table,
			RecommendedSQL: "ALTER TABLE " + table +
				" SET (autovacuum_vacuum_scale_factor = 0.01)"}
	}
	in := []Finding{perTable("public.zeta"), {
		Category: "memory_tuning", Severity: "info", ObjectIdentifier: "instance",
		Title:          "global",
		RecommendedSQL: "ALTER SYSTEM SET autovacuum_vacuum_scale_factor = 0.05",
	}, perTable("public.alpha")}
	got := DeduplicateFindings(in, 0, noopLog)
	if len(got) != 3 {
		t.Fatalf("expected 3 kept, got %d", len(got))
	}
	for _, f := range got {
		if f.Title != "global" {
			if _, ok := f.Detail["overridden_by_tables"]; ok {
				t.Errorf("per-table finding %q annotated", f.Title)
			}
			continue
		}
		tables, _ := f.Detail["overridden_by_tables"].([]string)
		if strings.Join(tables, ",") != "public.alpha,public.zeta" {
			t.Fatalf("overridden_by_tables = %#v, want [public.alpha public.zeta]", tables)
		}
	}
}
