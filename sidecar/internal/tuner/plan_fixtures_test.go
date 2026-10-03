package tuner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Heuristics tested against real EXPLAIN (FORMAT JSON) output captured by
// TestGeneratePlanFixtures (Phase 0 item 11). The catalog facts the
// heuristics need (table rows, usable btree indexes) come from the same
// server, in testdata/plans/catalog.json.

func loadPlanFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(planFixtureDir, name+".json"))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return raw
}

func loadFixtureFacts(t *testing.T) *CatalogFacts {
	t.Helper()
	var facts CatalogFacts
	if err := json.Unmarshal(loadPlanFixture(t, "catalog"), &facts); err != nil {
		t.Fatalf("catalog fixture: %v", err)
	}
	if facts.Tables["planfx.orders"] != 200000 {
		t.Fatalf("catalog fixture lost planfx.orders rows: %+v", facts.Tables)
	}
	return &facts
}

func scanFixture(t *testing.T, name string, minParallelRows int64) []PlanSymptom {
	t.Helper()
	syms, err := ScanPlan(loadPlanFixture(t, name),
		WithCatalogFacts(loadFixtureFacts(t), minParallelRows))
	if err != nil {
		t.Fatalf("scan %s: %v", name, err)
	}
	return syms
}

func symptomsOfKind(syms []PlanSymptom, kind SymptomKind) []PlanSymptom {
	var out []PlanSymptom
	for _, s := range syms {
		if s.Kind == kind {
			out = append(out, s)
		}
	}
	return out
}

const fixtureParallelRows = 100_000

// A seq scan whose filter matches the leading column of a valid btree
// index, on a large table at low selectivity, earns an IndexScan hint
// that names that index and the plan's real alias.
func TestFixture_SeqScanWithUsableIndex(t *testing.T) {
	syms := scanFixture(t, "seq_scan_indexed_filter", 0)
	seq := symptomsOfKind(syms, SymptomSeqScanWithIndex)
	if len(seq) != 1 {
		t.Fatalf("seq_scan_with_index symptoms = %+v, want 1", syms)
	}
	if seq[0].IndexName != "orders_customer_idx" || seq[0].Alias != "o" ||
		seq[0].RelationName != "orders" {
		t.Fatalf("symptom = %+v", seq[0])
	}
	rx := Prescribe(seq[0], TunerConfig{})
	if rx == nil || rx.HintDirective != "IndexScan(o orders_customer_idx)" {
		t.Fatalf("prescription = %+v", rx)
	}
}

// No usable index for the filter: no index hint (the old checkSeqScan
// flagged every Seq Scan).
func TestFixture_SeqScanWithoutUsableIndex(t *testing.T) {
	for _, name := range []string{
		"seq_scan_unindexed_filter",  // note has no index
		"seq_scan_expression_filter", // lower(email) cannot use the email index
		"seq_scan_low_selectivity",   // the index exists but the scan returns ~all rows
		"seq_scan_no_filter",         // a full scan needs every row
		"serial_seq_scan",            // LIKE '%ab%' is not a btree operator
	} {
		syms := scanFixture(t, name, 0)
		if got := symptomsOfKind(syms, SymptomSeqScanWithIndex); len(got) != 0 {
			t.Errorf("%s: unexpected index-scan symptom %+v", name, got)
		}
	}
}

// Without catalog facts nothing is known about indexes or table size, so
// neither catalog-dependent symptom is emitted.
func TestFixture_NoFactsNoCatalogSymptoms(t *testing.T) {
	syms, err := ScanPlan(loadPlanFixture(t, "seq_scan_indexed_filter"))
	if err != nil {
		t.Fatal(err)
	}
	if len(symptomsOfKind(syms, SymptomSeqScanWithIndex)) != 0 ||
		len(symptomsOfKind(syms, SymptomParallelDisabled)) != 0 {
		t.Fatalf("facts-free scan emitted catalog symptoms: %+v", syms)
	}
}

// A serial Seq Scan of a table at or above parallel_min_table_rows with no
// Gather above it is parallel-disabled; below the threshold it is not.
func TestFixture_ParallelDisabledOnSerialLargeScan(t *testing.T) {
	syms := scanFixture(t, "serial_seq_scan", fixtureParallelRows)
	par := symptomsOfKind(syms, SymptomParallelDisabled)
	if len(par) != 1 || par[0].RelationName != "orders" || par[0].Alias != "o" {
		t.Fatalf("parallel symptoms = %+v, want one on orders", syms)
	}
	if rows, _ := par[0].Detail["table_rows"].(int64); rows != 200000 {
		t.Fatalf("table_rows = %v, want 200000", par[0].Detail["table_rows"])
	}
	if got := symptomsOfKind(scanFixture(t, "serial_seq_scan", 200001),
		SymptomParallelDisabled); len(got) != 0 {
		t.Fatalf("table below threshold flagged: %+v", got)
	}
	if got := symptomsOfKind(scanFixture(t, "serial_seq_scan", 200000),
		SymptomParallelDisabled); len(got) != 1 {
		t.Fatalf("table exactly at threshold not flagged: %+v", got)
	}
}

// Scans under Gather / Gather Merge already run in parallel; a zero
// threshold disables the check.
func TestFixture_ParallelPlansNotFlagged(t *testing.T) {
	for _, name := range []string{"parallel_gather", "parallel_gather_merge"} {
		if got := symptomsOfKind(scanFixture(t, name, fixtureParallelRows),
			SymptomParallelDisabled); len(got) != 0 {
			t.Errorf("%s: parallel plan flagged %+v", name, got)
		}
	}
	if got := symptomsOfKind(scanFixture(t, "serial_seq_scan", 0),
		SymptomParallelDisabled); len(got) != 0 {
		t.Fatalf("zero threshold flagged %+v", got)
	}
}

// Small tables are never parallel candidates (customers: 5000 rows).
func TestFixture_ParallelIgnoresSmallTables(t *testing.T) {
	syms := scanFixture(t, "hash_join_spill", fixtureParallelRows)
	for _, s := range symptomsOfKind(syms, SymptomParallelDisabled) {
		if s.RelationName == "customers" {
			t.Fatalf("5000-row table flagged: %+v", s)
		}
	}
}

// A misestimated Nested Loop names the aliases of the relations it joins
// (from its children, never from the join node), so the hint is
// HashJoin(c o) and not HashJoin() / HashJoin(<one alias>).
func TestFixture_NestedLoopHashJoinUsesChildAliases(t *testing.T) {
	for _, name := range []string{"nested_loop_misestimate", "nested_loop_with_subplan"} {
		bnl := symptomsOfKind(scanFixture(t, name, 0), SymptomBadNestedLoop)
		if len(bnl) != 1 {
			t.Fatalf("%s: bad nested loop symptoms = %+v", name, bnl)
		}
		if !reflect.DeepEqual(bnl[0].JoinAliases, []string{"c", "o"}) {
			t.Fatalf("%s: aliases = %v, want [c o] (SubPlan c2 excluded)", name,
				bnl[0].JoinAliases)
		}
		rx := Prescribe(bnl[0], TunerConfig{})
		if rx == nil || rx.HintDirective != "HashJoin(c o)" {
			t.Fatalf("%s: prescription = %+v", name, rx)
		}
	}
}

func TestFixture_SpillsAndSortLimit(t *testing.T) {
	if got := symptomsOfKind(scanFixture(t, "hash_join_spill", 0),
		SymptomHashSpill); len(got) != 1 || got[0].Detail["hash_batches"] != int64(4) {
		t.Fatalf("hash spill = %+v", got)
	}
	if got := symptomsOfKind(scanFixture(t, "disk_sort", 0),
		SymptomDiskSort); len(got) != 1 {
		t.Fatalf("disk sort = %+v", got)
	}
	if got := symptomsOfKind(scanFixture(t, "sort_under_limit", 0),
		SymptomSortLimit); len(got) != 1 {
		t.Fatalf("sort under limit = %+v", got)
	}
}

// End to end over every fixture: no prescription is ever an empty or
// unparseable hint, and every hint names only plan aliases.
func TestFixture_NoEmptyOrInvalidHints(t *testing.T) {
	for _, spec := range planFixtureSpecs {
		syms := scanFixture(t, spec.name, fixtureParallelRows)
		for _, s := range syms {
			rx := Prescribe(s, TunerConfig{WorkMemMaxMB: 1024})
			if rx == nil || rx.HintDirective == "" {
				continue
			}
			if !validateHintSyntax(rx.HintDirective) ||
				strings.Contains(rx.HintDirective, "()") {
				t.Errorf("%s: invalid hint %q for %+v", spec.name, rx.HintDirective, s)
			}
		}
	}
}
