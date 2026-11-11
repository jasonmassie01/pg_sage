package tuner

import (
	"strings"
	"sync"
	"testing"
)

// tuning-10: Describe turns a plan symptom into the deterministic
// "slow because" and "recommend" lines of the explain floor.

func TestDescribe_DiskSortNamesSpillInMB(t *testing.T) {
	because, recommend := Describe(PlanSymptom{Kind: SymptomDiskSort, NodeType: "Sort",
		Detail: map[string]any{"sort_space_kb": int64(8396)}})
	if because != "sort spilled 8.2 MB to disk" {
		t.Errorf("because = %q, want %q", because, "sort spilled 8.2 MB to disk")
	}
	if !strings.Contains(recommend, "work_mem") {
		t.Errorf("recommend = %q, want a work_mem recommendation", recommend)
	}
}

func TestDescribe_DiskSortSizeBoundary(t *testing.T) {
	cases := []struct {
		kb   int64
		want string
	}{
		{0, "sort spilled to disk"},
		{1, "sort spilled 1 KB to disk"},
		{1023, "sort spilled 1023 KB to disk"},
		{1024, "sort spilled 1.0 MB to disk"},
		{1024 * 1024, "sort spilled 1.0 GB to disk"},
	}
	for _, c := range cases {
		because, _ := Describe(PlanSymptom{Kind: SymptomDiskSort,
			Detail: map[string]any{"sort_space_kb": c.kb}})
		if because != c.want {
			t.Errorf("kb=%d: because = %q, want %q", c.kb, because, c.want)
		}
	}
}

func TestDescribe_HashSpillNamesBatches(t *testing.T) {
	because, recommend := Describe(PlanSymptom{Kind: SymptomHashSpill, NodeType: "Hash",
		Detail: map[string]any{"hash_batches": int64(16), "peak_memory_kb": int64(4096)}})
	if !strings.Contains(because, "16 batches") || !strings.Contains(because, "4.0 MB") {
		t.Errorf("because = %q, want the batch count and peak memory", because)
	}
	if !strings.Contains(recommend, "work_mem") {
		t.Errorf("recommend = %q, want a work_mem recommendation", recommend)
	}
}

func TestDescribe_BadNestedLoopNamesInnerRows(t *testing.T) {
	because, recommend := Describe(PlanSymptom{Kind: SymptomBadNestedLoop,
		NodeType: "Nested Loop", JoinAliases: []string{"o", "c"},
		Detail: map[string]any{"inner_rows_total": int64(500000), "loops": int64(5000)}})
	if !strings.Contains(because, "500000 rows") || !strings.Contains(because, "5000 loops") {
		t.Errorf("because = %q, want inner rows and loops", because)
	}
	if !strings.Contains(recommend, "ANALYZE") {
		t.Errorf("recommend = %q, want ANALYZE first", recommend)
	}
}

func TestDescribe_SeqScanWithIndexNamesRelationAndIndex(t *testing.T) {
	because, recommend := Describe(PlanSymptom{Kind: SymptomSeqScanWithIndex,
		NodeType: "Seq Scan", RelationName: "orders", IndexName: "orders_customer_idx",
		Detail: map[string]any{"table_rows": int64(200000), "rows": int64(40)}})
	if !strings.Contains(because, "orders") || !strings.Contains(because, "orders_customer_idx") {
		t.Errorf("because = %q, want the relation and the index", because)
	}
	if !strings.Contains(recommend, "orders_customer_idx") {
		t.Errorf("recommend = %q, want the index named", recommend)
	}
}

// Every symptom the plan scan can emit has a non-empty description, so no
// rule finding is silently dropped from the floor.
func TestDescribe_EveryKindHasBecauseAndRecommend(t *testing.T) {
	kinds := []SymptomKind{SymptomDiskSort, SymptomHashSpill, SymptomHighPlanTime,
		SymptomBadNestedLoop, SymptomSeqScanWithIndex, SymptomParallelDisabled,
		SymptomSortLimit, SymptomStatTempSpill, SymptomStaleStats,
		SymptomIndexFilterInefficient, SymptomORJoinFilter}
	for _, k := range kinds {
		because, recommend := Describe(PlanSymptom{Kind: k, RelationName: "t"})
		if because == "" || recommend == "" {
			t.Errorf("%s: because=%q recommend=%q, want both non-empty", k, because, recommend)
		}
		if strings.Contains(because, "%!") || strings.Contains(recommend, "%!") {
			t.Errorf("%s: formatting error in %q / %q", k, because, recommend)
		}
	}
}

// Nil/empty: a symptom with no detail still describes itself without
// inventing numbers.
func TestDescribe_NilDetail(t *testing.T) {
	because, _ := Describe(PlanSymptom{Kind: SymptomSortLimit})
	if because == "" || strings.Contains(because, " 0 rows") {
		t.Errorf("because = %q, want a description without a made-up row count", because)
	}
}

// Invalid input: an unknown kind is named, not dropped.
func TestDescribe_UnknownKind(t *testing.T) {
	because, recommend := Describe(PlanSymptom{Kind: "made_up", NodeType: "Seq Scan"})
	if !strings.Contains(because, "made_up") {
		t.Errorf("because = %q, want the unknown kind named", because)
	}
	if recommend != "" {
		t.Errorf("recommend = %q, want none for an unknown kind", recommend)
	}
}

// Wrong-typed detail values are ignored rather than mis-printed.
func TestDescribe_WrongTypedDetail(t *testing.T) {
	because, _ := Describe(PlanSymptom{Kind: SymptomDiskSort,
		Detail: map[string]any{"sort_space_kb": "lots"}})
	if because != "sort spilled to disk" {
		t.Errorf("because = %q, want the size left out", because)
	}
}

// Concurrent access: Describe is pure; parallel calls agree.
func TestDescribe_Concurrent(t *testing.T) {
	s := PlanSymptom{Kind: SymptomDiskSort, Detail: map[string]any{"sort_space_kb": int64(2048)}}
	var wg sync.WaitGroup
	got := make([]string, 8)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], _ = Describe(s)
		}(i)
	}
	wg.Wait()
	for i, g := range got {
		if g != "sort spilled 2.0 MB to disk" {
			t.Errorf("call %d: %q", i, g)
		}
	}
}

// PlanRelationNames is exported for the explain floor's catalog facts.
func TestPlanRelationNames_Exported(t *testing.T) {
	names, err := PlanRelationNames([]byte(`[{"Plan":{"Node Type":"Hash Join","Plans":[
		{"Node Type":"Seq Scan","Relation Name":"Orders"},
		{"Node Type":"Seq Scan","Relation Name":"customers"}]}}]`))
	if err != nil {
		t.Fatalf("PlanRelationNames: %v", err)
	}
	if strings.Join(names, ",") != "Orders,customers" {
		t.Errorf("names = %v, want [Orders customers] (catalog spelling kept)", names)
	}
}
