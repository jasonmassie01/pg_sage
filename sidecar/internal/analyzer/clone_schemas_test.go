package analyzer

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// Dogfood lifeos-1 finding 7: 477 of 478 open duplicate_index findings
// were in leaked test schemas (test_memory_<hash>, ...), copies of one
// schema. Schemas of one shape whose names differ only by a generated
// suffix are a clone family: their findings are reported once per family,
// not once per schema. No schema name is special-cased.

func cloneSnapshot(prefix string, n int, extra ...string) *collector.Snapshot {
	s := &collector.Snapshot{}
	add := func(schema string, tables ...string) {
		for _, tb := range tables {
			s.Tables = append(s.Tables, collector.TableStats{SchemaName: schema, RelName: tb})
		}
	}
	for i := 0; i < n; i++ {
		add(fmt.Sprintf("%s%032x", prefix, i+1), "thesis_allocation", "runs")
	}
	add("public", "thesis_allocation", "runs", "users")
	for _, schema := range extra {
		add(schema, "thesis_allocation", "runs")
	}
	return s
}

func dupFindingIn(schema string) Finding {
	return dropFinding("duplicate_index", schema+".idx_thesis_allocation_run")
}

func TestCollapseCloneSchemas_OneFindingPerFamily(t *testing.T) {
	snap := cloneSnapshot("test_memory_", 6)
	var in []Finding
	for _, tb := range snap.Tables {
		if strings.HasPrefix(tb.SchemaName, "test_memory_") && tb.RelName == "runs" {
			in = append(in, dupFindingIn(tb.SchemaName))
			in = append(in, Finding{Category: "unused_index",
				ObjectIdentifier: tb.SchemaName + ".runs_pkey", RecommendedSQL: "DROP ..."})
		}
	}
	in = append(in, dupFindingIn("public"))
	out := collapseCloneSchemas(snap, in, idleSignals())
	var family []Finding
	others := 0
	for _, f := range out {
		switch {
		case f.Category == CategoryCloneSchemas:
			family = append(family, f)
		case strings.HasPrefix(f.ObjectIdentifier, "test_memory_"):
			t.Fatalf("a clone-schema finding survived: %+v", f)
		default:
			others++
		}
	}
	if len(family) != 1 || others != 1 {
		t.Fatalf("family findings = %d, others = %d; want 1 and the public one", len(family),
			others)
	}
	f := family[0]
	if f.RecommendedSQL != "" || f.Detail["schemas"] != 6 ||
		f.Detail["collapsed_findings"] != 12 ||
		!strings.Contains(f.ObjectIdentifier, "test_memory_") ||
		!strings.Contains(f.Title, "6 schemas") {
		t.Fatalf("family finding = %+v", f)
	}
	counts, _ := f.Detail["by_category"].(map[string]int)
	if counts["duplicate_index"] != 6 || counts["unused_index"] != 6 {
		t.Fatalf("by_category = %v", f.Detail["by_category"])
	}
}

// Boundaries: fewer than cloneFamilyMin copies are not a family; schemas
// with the same tables but unrelated names (no generated suffix) are
// not collapsed; a schema with an extra table is not part of the family.
func TestCollapseCloneSchemas_Boundaries(t *testing.T) {
	small := cloneSnapshot("tmp_", cloneFamilyMin-1)
	var in []Finding
	for i := 0; i < cloneFamilyMin-1; i++ {
		in = append(in, dupFindingIn(fmt.Sprintf("tmp_%032x", i+1)))
	}
	if out := collapseCloneSchemas(small, in, idleSignals()); len(out) != len(in) {
		t.Fatalf("%d copies collapsed into %d findings", cloneFamilyMin-1, len(out))
	}
	named := cloneSnapshot("x_", 0, "sales", "billing", "audit", "crm", "ops", "hr")
	var named2 []Finding
	for _, s := range []string{"sales", "billing", "audit", "crm", "ops", "hr"} {
		named2 = append(named2, dupFindingIn(s))
	}
	if out := collapseCloneSchemas(named, named2, idleSignals()); len(out) != len(named2) {
		t.Fatalf("hand-named schemas collapsed: %+v", out)
	}
}

func TestCollapseCloneSchemas_EmptyInputs(t *testing.T) {
	if out := collapseCloneSchemas(nil, nil, idleSignals()); len(out) != 0 {
		t.Fatalf("nil = %+v", out)
	}
	in := []Finding{dupFindingIn("public")}
	if out := collapseCloneSchemas(&collector.Snapshot{}, in, idleSignals()); len(out) != 1 {
		t.Fatalf("empty snapshot changed findings: %+v", out)
	}
}

func TestCloneStem(t *testing.T) {
	cases := map[string]string{
		"test_memory_f5bec6e92e5144e18703b2a355b97b9e": "test_memory_",
		"test_attention_0a19ba71c91a4e8ab50257edc2191ec0": "test_attention_",
		"tenant_000123": "tenant_",
		"run-20261002-1830": "run-",
		"public":         "",
		"sales":          "",
		"v2":             "",
		"schema_abc":     "", // too short to be a generated suffix
	}
	for in, want := range cases {
		if got := cloneStem(in); got != want {
			t.Errorf("cloneStem(%q) = %q, want %q", in, got, want)
		}
	}
}
