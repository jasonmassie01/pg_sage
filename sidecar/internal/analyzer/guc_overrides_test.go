package analyzer

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// advisors-02: a global setting finding names the tables that keep their
// own value of it.

// Tables with an explicit storage parameter join overridden_by_tables of
// the global finding of that GUC; a nil snapshot and per-table findings
// are left alone.
func TestAnnotateReloptionOverrides(t *testing.T) {
	global := Finding{Category: "memory_tuning", ObjectIdentifier: "instance",
		RecommendedSQL: "ALTER SYSTEM SET autovacuum_vacuum_scale_factor = 0.05",
		Detail:         map[string]any{DetailOverriddenByTables: []string{"public.t1"}}}
	perTable := Finding{Category: "vacuum_tuning", ObjectIdentifier: "public.t1",
		RecommendedSQL: "ALTER TABLE public.t1 SET (autovacuum_vacuum_scale_factor = 0.01)"}
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		{SchemaName: "public", RelName: "t2",
			Reloptions: []string{"autovacuum_vacuum_scale_factor=0.02", "fillfactor=90"}},
		{SchemaName: "public", RelName: "t1",
			Reloptions: []string{"autovacuum_vacuum_scale_factor=0.01"}},
		{SchemaName: "public", RelName: "bad", Reloptions: []string{"=x", "novalue"}},
	}}
	kept := annotateReloptionOverrides([]Finding{global}, nil)
	if prior, _ := kept[0].Detail[DetailOverriddenByTables].([]string); len(prior) != 1 ||
		prior[0] != "public.t1" {
		t.Fatalf("nil snapshot changed the finding: %+v", kept)
	}
	got := annotateReloptionOverrides([]Finding{global, perTable}, snap)
	tables, _ := got[0].Detail[DetailOverriddenByTables].([]string)
	if strings.Join(tables, ",") != "public.t1,public.t2" {
		t.Fatalf("overridden_by_tables = %#v, want [public.t1 public.t2]", tables)
	}
	if strings.Count(got[0].Recommendation, overrideRationale) != 1 {
		t.Fatalf("rationale = %q, want the override line once", got[0].Recommendation)
	}
	if _, ok := got[1].Detail[DetailOverriddenByTables]; ok {
		t.Fatal("per-table finding annotated")
	}
	if prior := global.Detail[DetailOverriddenByTables].([]string); len(prior) != 1 {
		t.Fatalf("input detail mutated: %v", prior)
	}
}
