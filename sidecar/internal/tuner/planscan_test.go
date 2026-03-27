package tuner

import (
	"testing"
)

func TestScanPlan_DiskSort(t *testing.T) {
	plan := `[{"Plan": {
		"Node Type": "Sort",
		"Plan Rows": 1000,
		"Sort Method": "external merge",
		"Sort Space Used": 4096,
		"Sort Space Type": "Disk"
	}}]`
	syms, err := ScanPlan([]byte(plan))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(syms) == 0 {
		t.Fatal("expected disk_sort symptom, got none")
	}
	found := false
	for _, s := range syms {
		if s.Kind == SymptomDiskSort {
			found = true
			kb, ok := s.Detail["sort_space_kb"].(int64)
			if !ok || kb != 4096 {
				t.Errorf("sort_space_kb = %v, want 4096", kb)
			}
		}
	}
	if !found {
		t.Error("disk_sort symptom not found")
	}
}

func TestScanPlan_HashSpill(t *testing.T) {
	plan := `[{"Plan": {
		"Node Type": "Hash Join",
		"Plan Rows": 500,
		"Hash Batches": 16,
		"Peak Memory Usage": 8192,
		"Plans": [
			{"Node Type": "Seq Scan", "Plan Rows": 100,
			 "Relation Name": "t1", "Alias": "t1"},
			{"Node Type": "Hash", "Plan Rows": 200,
			 "Plans": [
				{"Node Type": "Seq Scan", "Plan Rows": 200,
				 "Relation Name": "t2", "Alias": "t2"}
			]}
		]
	}}]`
	syms, err := ScanPlan([]byte(plan))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, s := range syms {
		if s.Kind == SymptomHashSpill {
			found = true
			b, _ := s.Detail["hash_batches"].(int64)
			if b != 16 {
				t.Errorf("hash_batches = %d, want 16", b)
			}
		}
	}
	if !found {
		t.Error("hash_spill symptom not found")
	}
}

func TestScanPlan_BadNestedLoop(t *testing.T) {
	actual := int64(50000)
	_ = actual // used in JSON below
	plan := `[{"Plan": {
		"Node Type": "Nested Loop",
		"Plan Rows": 10,
		"Actual Rows": 50000,
		"Alias": "nl1",
		"Plans": [
			{"Node Type": "Index Scan", "Plan Rows": 1,
			 "Relation Name": "orders",
			 "Index Name": "orders_pkey"}
		]
	}}]`
	syms, err := ScanPlan([]byte(plan))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, s := range syms {
		if s.Kind == SymptomBadNestedLoop {
			found = true
			if s.Alias != "nl1" {
				t.Errorf("alias = %q, want nl1", s.Alias)
			}
		}
	}
	if !found {
		t.Error("bad_nested_loop symptom not found")
	}
}

func TestScanPlan_SeqScanNamed(t *testing.T) {
	plan := `[{"Plan": {
		"Node Type": "Seq Scan",
		"Plan Rows": 5000,
		"Relation Name": "users",
		"Schema": "public",
		"Alias": "u"
	}}]`
	syms, err := ScanPlan([]byte(plan))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var seqScan, parDisabled bool
	for _, s := range syms {
		if s.Kind == SymptomSeqScanWithIndex {
			seqScan = true
			if s.RelationName != "users" {
				t.Errorf("relation = %q, want users",
					s.RelationName)
			}
		}
		if s.Kind == SymptomParallelDisabled {
			parDisabled = true
		}
	}
	if !seqScan {
		t.Error("seq_scan_with_index symptom not found")
	}
	// Seq Scan contains "Scan" and no WorkersPlanned
	if !parDisabled {
		t.Error("parallel_disabled also expected")
	}
}

func TestScanPlan_ParallelNotFlagged(t *testing.T) {
	plan := `[{"Plan": {
		"Node Type": "Gather",
		"Plan Rows": 10000,
		"Workers Planned": 2,
		"Workers Launched": 2,
		"Plans": [{
			"Node Type": "Parallel Seq Scan",
			"Plan Rows": 5000,
			"Relation Name": "big_table",
			"Alias": "bt",
			"Workers Planned": 2
		}]
	}}]`
	syms, err := ScanPlan([]byte(plan))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, s := range syms {
		if s.Kind == SymptomParallelDisabled {
			t.Error("should not flag parallel_disabled " +
				"when WorkersPlanned is set")
		}
	}
}

func TestScanPlan_CleanPlan(t *testing.T) {
	plan := `[{"Plan": {
		"Node Type": "Index Scan",
		"Plan Rows": 1,
		"Relation Name": "users",
		"Index Name": "users_pkey",
		"Workers Planned": 0
	}}]`
	syms, err := ScanPlan([]byte(plan))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(syms) != 0 {
		t.Errorf("expected 0 symptoms, got %d: %v",
			len(syms), syms)
	}
}

func TestScanPlan_NestedSymptoms(t *testing.T) {
	plan := `[{"Plan": {
		"Node Type": "Hash Join",
		"Plan Rows": 100,
		"Plans": [
			{"Node Type": "Sort", "Plan Rows": 500,
			 "Sort Space Used": 2048,
			 "Sort Space Type": "Disk",
			 "Plans": [
				{"Node Type": "Seq Scan", "Plan Rows": 500,
				 "Relation Name": "items", "Alias": "i"}
			]},
			{"Node Type": "Hash", "Plan Rows": 50,
			 "Plans": [
				{"Node Type": "Seq Scan", "Plan Rows": 50,
				 "Relation Name": "cats", "Alias": "c"}
			]}
		]
	}}]`
	syms, err := ScanPlan([]byte(plan))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var hasDisk, hasSeq bool
	for _, s := range syms {
		if s.Kind == SymptomDiskSort {
			hasDisk = true
			if s.NodeDepth != 1 {
				t.Errorf("disk sort depth = %d, want 1",
					s.NodeDepth)
			}
		}
		if s.Kind == SymptomSeqScanWithIndex {
			hasSeq = true
		}
	}
	if !hasDisk {
		t.Error("expected disk_sort in child node")
	}
	if !hasSeq {
		t.Error("expected seq_scan_with_index in child")
	}
}

func TestScanPlan_SortLimit(t *testing.T) {
	plan := `[{"Plan": {
		"Node Type": "Limit",
		"Plan Rows": 10,
		"Plans": [{
			"Node Type": "Sort",
			"Plan Rows": 1000000,
			"Sort Method": "top-N heapsort",
			"Sort Space Type": "Memory"
		}]
	}}]`
	syms, err := ScanPlan([]byte(plan))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, s := range syms {
		if s.Kind == SymptomSortLimit {
			found = true
			sr, _ := s.Detail["sort_rows"].(int64)
			lr, _ := s.Detail["limit_rows"].(int64)
			if sr != 1000000 {
				t.Errorf("sort_rows = %d, want 1000000", sr)
			}
			if lr != 10 {
				t.Errorf("limit_rows = %d, want 10", lr)
			}
		}
	}
	if !found {
		t.Error("sort_limit symptom not found")
	}
}

func TestScanPlan_SortLimit_NotTriggered(t *testing.T) {
	// Sort rows only 5x limit — below 10x threshold.
	plan := `[{"Plan": {
		"Node Type": "Limit",
		"Plan Rows": 100,
		"Plans": [{
			"Node Type": "Sort",
			"Plan Rows": 500
		}]
	}}]`
	syms, err := ScanPlan([]byte(plan))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, s := range syms {
		if s.Kind == SymptomSortLimit {
			t.Error("should not flag sort_limit for 5x ratio")
		}
	}
}

func TestScanPlan_MalformedJSON(t *testing.T) {
	_, err := ScanPlan([]byte(`{not json`))
	if err == nil {
		t.Error("expected error for malformed JSON")
	}
}

func TestScanPlan_EmptyPlan(t *testing.T) {
	syms, err := ScanPlan([]byte(`[{"Plan": {}}]`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(syms) != 0 {
		t.Errorf("expected 0 symptoms, got %d", len(syms))
	}
}

// TestScanPlan_CorrelatedSubqueryDeepNesting verifies that walkNodeWithParent
// visits inner nodes at depth 3+ and detects symptoms there. The fixture
// models a correlated subquery: Nested Loop drives an inner Sort (disk spill)
// over a Hash Join over two Seq Scans.
func TestScanPlan_CorrelatedSubqueryDeepNesting(t *testing.T) {
	plan := `[{"Plan": {
		"Node Type": "Nested Loop",
		"Join Type": "Inner",
		"Plan Rows": 5,
		"Actual Rows": 75000,
		"Actual Loops": 1,
		"Plans": [
			{
				"Node Type": "Index Scan",
				"Plan Rows": 500,
				"Relation Name": "customers",
				"Schema": "public",
				"Alias": "c",
				"Index Name": "customers_pkey",
				"Workers Planned": 0
			},
			{
				"Node Type": "Sort",
				"Plan Rows": 15000,
				"Sort Method": "external merge",
				"Sort Space Used": 32768,
				"Sort Space Type": "Disk",
				"Plans": [
					{
						"Node Type": "Hash Join",
						"Join Type": "Inner",
						"Plan Rows": 15000,
						"Hash Batches": 32,
						"Original Hash Batches": 4,
						"Peak Memory Usage": 16384,
						"Plans": [
							{
								"Node Type": "Seq Scan",
								"Plan Rows": 200000,
								"Relation Name": "orders",
								"Schema": "public",
								"Alias": "o",
								"Filter": "(o.customer_id = c.id)",
								"Rows Removed by Filter": 185000
							},
							{
								"Node Type": "Hash",
								"Plan Rows": 50000,
								"Plans": [
									{
										"Node Type": "Seq Scan",
										"Plan Rows": 50000,
										"Relation Name": "line_items",
										"Schema": "public",
										"Alias": "li"
									}
								]
							}
						]
					}
				]
			}
		]
	}}]`

	syms, err := ScanPlan([]byte(plan))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Collect symptom kinds with their depths for verification.
	type found struct {
		kind  SymptomKind
		depth int
	}
	var hits []found
	for _, s := range syms {
		hits = append(hits, found{s.Kind, s.NodeDepth})
	}

	// Helper: assert a symptom exists at a specific depth.
	assertSymptom := func(kind SymptomKind, wantDepth int) {
		t.Helper()
		for _, h := range hits {
			if h.kind == kind && h.depth == wantDepth {
				return
			}
		}
		t.Errorf("symptom %q at depth %d not found; got %v",
			kind, wantDepth, hits)
	}

	// bad_nested_loop at root (depth 0): planned 5, actual 75000
	assertSymptom(SymptomBadNestedLoop, 0)

	// disk_sort at depth 1 (inner side of Nested Loop)
	assertSymptom(SymptomDiskSort, 1)

	// hash_spill at depth 2 (Hash Join under Sort)
	assertSymptom(SymptomHashSpill, 2)

	// seq_scan_with_index on "orders" at depth 3
	assertSymptom(SymptomSeqScanWithIndex, 3)

	// seq_scan_with_index on "line_items" at depth 4
	// (Hash is depth 3, Seq Scan under it is depth 4)
	assertSymptom(SymptomSeqScanWithIndex, 4)

	// Verify detail values on specific symptoms.
	for _, s := range syms {
		switch {
		case s.Kind == SymptomDiskSort && s.NodeDepth == 1:
			kb, ok := s.Detail["sort_space_kb"].(int64)
			if !ok || kb != 32768 {
				t.Errorf("disk sort space = %v, want 32768", kb)
			}
		case s.Kind == SymptomHashSpill && s.NodeDepth == 2:
			b, _ := s.Detail["hash_batches"].(int64)
			if b != 32 {
				t.Errorf("hash_batches = %d, want 32", b)
			}
			pk, _ := s.Detail["peak_memory_kb"].(int64)
			if pk != 16384 {
				t.Errorf("peak_memory_kb = %d, want 16384", pk)
			}
		case s.Kind == SymptomBadNestedLoop && s.NodeDepth == 0:
			ar, _ := s.Detail["actual_rows"].(int64)
			if ar != 75000 {
				t.Errorf("actual_rows = %d, want 75000", ar)
			}
			pr, _ := s.Detail["plan_rows"].(int64)
			if pr != 5 {
				t.Errorf("plan_rows = %d, want 5", pr)
			}
		}
	}

	// Sanity: we should have at least 7 symptoms total:
	// 1 bad_nested_loop + 1 disk_sort + 1 hash_spill +
	// 2 seq_scan + 2+ parallel_disabled (for seq scans without
	// WorkersPlanned).
	if len(syms) < 7 {
		t.Errorf("expected >= 7 symptoms, got %d", len(syms))
	}
}

// TestScanPlan_ExistsSubPlanDiskSort verifies that ScanPlan
// detects a disk Sort inside an EXISTS subquery's SubPlan node.
// PostgreSQL marks these with "Subplan Name": "SubPlan N".
func TestScanPlan_ExistsSubPlanDiskSort(t *testing.T) {
	plan := `[{"Plan": {
		"Node Type": "Nested Loop",
		"Join Type": "Semi",
		"Plan Rows": 100,
		"Plans": [
			{
				"Node Type": "Seq Scan",
				"Plan Rows": 1000,
				"Relation Name": "customers",
				"Schema": "public",
				"Alias": "c",
				"Workers Planned": 0
			},
			{
				"Node Type": "Sort",
				"Subplan Name": "SubPlan 1",
				"Plan Rows": 50000,
				"Sort Method": "external merge",
				"Sort Space Used": 65536,
				"Sort Space Type": "Disk",
				"Plans": [{
					"Node Type": "Seq Scan",
					"Plan Rows": 50000,
					"Relation Name": "orders",
					"Schema": "public",
					"Alias": "o"
				}]
			}
		]
	}}]`

	syms, err := ScanPlan([]byte(plan))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var diskSort, seqOrders bool
	for _, s := range syms {
		if s.Kind == SymptomDiskSort && s.NodeDepth == 1 {
			diskSort = true
			kb, _ := s.Detail["sort_space_kb"].(int64)
			if kb != 65536 {
				t.Errorf("sort_space_kb = %d, want 65536", kb)
			}
		}
		if s.Kind == SymptomSeqScanWithIndex &&
			s.RelationName == "orders" {
			seqOrders = true
		}
	}
	if !diskSort {
		t.Error("disk_sort not found inside SubPlan")
	}
	if !seqOrders {
		t.Error("seq_scan on orders not found inside SubPlan")
	}
}

func TestScanPlan_BareObjectFormat(t *testing.T) {
	plan := `{"Plan": {
		"Node Type": "Sort",
		"Plan Rows": 100,
		"Sort Space Used": 1024,
		"Sort Space Type": "Disk"
	}}`
	syms, err := ScanPlan([]byte(plan))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, s := range syms {
		if s.Kind == SymptomDiskSort {
			found = true
		}
	}
	if !found {
		t.Error("disk_sort not found in bare object format")
	}
}
