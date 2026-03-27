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
