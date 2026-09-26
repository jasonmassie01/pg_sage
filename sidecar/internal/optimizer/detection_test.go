package optimizer

import "testing"

// ---------------------------------------------------------------------------
// DetectIncludeCandidates
// ---------------------------------------------------------------------------

func TestDetectIncludeCandidates_IndexScanHighHeap(t *testing.T) {
	plans := []PlanSummary{
		{QueryID: 1, ScanType: "Index Scan", HeapFetches: 2000},
	}
	got := DetectIncludeCandidates(plans, 1000)
	if len(got) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(got))
	}
	if got[0].QueryID != 1 {
		t.Fatalf("expected QueryID=1, got %d", got[0].QueryID)
	}
	if got[0].HeapFetches != 2000 {
		t.Fatalf("expected HeapFetches=2000, got %d", got[0].HeapFetches)
	}
}

func TestDetectIncludeCandidates_SeqScanIgnored(t *testing.T) {
	plans := []PlanSummary{
		{QueryID: 1, ScanType: "Seq Scan", HeapFetches: 5000},
	}
	got := DetectIncludeCandidates(plans, 1000)
	if len(got) != 0 {
		t.Fatalf("expected 0 candidates for Seq Scan, got %d", len(got))
	}
}

func TestDetectIncludeCandidates_BelowThreshold(t *testing.T) {
	plans := []PlanSummary{
		{QueryID: 1, ScanType: "Index Scan", HeapFetches: 500},
	}
	got := DetectIncludeCandidates(plans, 1000)
	if len(got) != 0 {
		t.Fatalf("expected 0 candidates when HeapFetches < threshold, got %d", len(got))
	}
}

func TestDetectIncludeCandidates_EmptyPlans(t *testing.T) {
	got := DetectIncludeCandidates(nil, 1000)
	if len(got) != 0 {
		t.Fatalf("expected 0 candidates for nil plans, got %d", len(got))
	}
}

func TestDetectIncludeCandidates_IndexOnlyScanIgnored(t *testing.T) {
	plans := []PlanSummary{
		{QueryID: 1, ScanType: "Index Only Scan", HeapFetches: 3000},
	}
	got := DetectIncludeCandidates(plans, 1000)
	if len(got) != 0 {
		t.Fatalf(
			"expected 0 candidates for Index Only Scan, got %d", len(got),
		)
	}
}

// ---------------------------------------------------------------------------
// DetectPartialCandidates
// ---------------------------------------------------------------------------

func TestDetectPartialCandidates_HighFrequencyFilter(t *testing.T) {
	queries := []QueryInfo{
		{QueryID: 1, Text: "SELECT * FROM t WHERE status = 'active'"},
		{QueryID: 2, Text: "SELECT * FROM t WHERE status = 'active'"},
		{QueryID: 3, Text: "SELECT * FROM t WHERE status = 'active'"},
		{QueryID: 4, Text: "SELECT * FROM t WHERE status = 'active'"},
		{QueryID: 5, Text: "SELECT * FROM t WHERE name = 'bob'"},
	}
	colStats := []ColStat{
		{Column: "status", MostCommonFreqs: []float64{0.05}},
	}
	got := DetectPartialCandidates(queries, colStats)
	if len(got) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(got))
	}
	if got[0].Column != "status" {
		t.Fatalf("expected column=status, got %s", got[0].Column)
	}
	if got[0].FilterValue != "active" {
		t.Fatalf("expected filter=active, got %s", got[0].FilterValue)
	}
	if got[0].QueryPct < 0.79 || got[0].QueryPct > 0.81 {
		t.Fatalf("expected ~0.80 query pct, got %f", got[0].QueryPct)
	}
	if got[0].Selectivity != 0.05 {
		t.Fatalf("expected selectivity=0.05, got %f", got[0].Selectivity)
	}
}

func TestDetectPartialCandidates_LowFrequencyFilter(t *testing.T) {
	queries := []QueryInfo{
		{QueryID: 1, Text: "SELECT * FROM t WHERE status = 'active'"},
		{QueryID: 2, Text: "SELECT * FROM t WHERE status = 'active'"},
		{QueryID: 3, Text: "SELECT * FROM t WHERE name = 'bob'"},
		{QueryID: 4, Text: "SELECT * FROM t WHERE age = '30'"},
		{QueryID: 5, Text: "SELECT * FROM t WHERE id = '1'"},
	}
	colStats := []ColStat{
		{Column: "status", MostCommonFreqs: []float64{0.05}},
	}
	got := DetectPartialCandidates(queries, colStats)
	if len(got) != 0 {
		t.Fatalf("expected 0 candidates at 40%% frequency, got %d", len(got))
	}
}

func TestDetectPartialCandidates_LowSelectivityConfirmed(t *testing.T) {
	queries := []QueryInfo{
		{QueryID: 1, Text: "SELECT * FROM t WHERE status = 'active'"},
		{QueryID: 2, Text: "SELECT * FROM t WHERE status = 'active'"},
		{QueryID: 3, Text: "SELECT * FROM t WHERE status = 'active'"},
		{QueryID: 4, Text: "SELECT * FROM t WHERE status = 'active'"},
		{QueryID: 5, Text: "SELECT * FROM t WHERE status = 'active'"},
	}
	colStats := []ColStat{
		{Column: "status", MostCommonFreqs: []float64{0.05}},
	}
	got := DetectPartialCandidates(queries, colStats)
	if len(got) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(got))
	}
	if got[0].Selectivity != 0.05 {
		t.Fatalf("expected selectivity=0.05, got %f", got[0].Selectivity)
	}
}

func TestDetectPartialCandidates_EmptyQueries(t *testing.T) {
	got := DetectPartialCandidates(nil, nil)
	if got != nil {
		t.Fatalf("expected nil for empty queries, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// DetectJoinPairs
// ---------------------------------------------------------------------------

func TestDetectJoinPairs_ExplicitJoin(t *testing.T) {
	queries := []QueryInfo{
		{
			QueryID: 1,
			Text: "SELECT * FROM customers " +
				"JOIN orders ON orders.customer_id = customers.id " +
				"WHERE customers.active = true",
		},
	}
	got := DetectJoinPairs(queries)
	if len(got) != 1 {
		t.Fatalf("expected 1 join pair, got %d", len(got))
	}
	if got[0].Left != "customers" && got[0].Right != "customers" {
		t.Fatalf(
			"expected customers in pair, got left=%s right=%s",
			got[0].Left, got[0].Right,
		)
	}
	if got[0].Left != "orders" && got[0].Right != "orders" {
		t.Fatalf(
			"expected orders in pair, got left=%s right=%s",
			got[0].Left, got[0].Right,
		)
	}
	if len(got[0].Queries) != 1 {
		t.Fatalf("expected 1 query in pair, got %d", len(got[0].Queries))
	}
}

func TestDetectJoinPairs_MultipleQueriesSameTables(t *testing.T) {
	queries := []QueryInfo{
		{
			QueryID: 1,
			Text: "SELECT * FROM customers " +
				"JOIN orders ON orders.customer_id = customers.id " +
				"WHERE 1=1",
		},
		{
			QueryID: 2,
			Text: "SELECT * FROM customers " +
				"JOIN orders ON orders.user_id = customers.id " +
				"WHERE 1=1",
		},
	}
	got := DetectJoinPairs(queries)
	if len(got) != 1 {
		t.Fatalf("expected 1 grouped pair, got %d", len(got))
	}
	if len(got[0].Queries) != 2 {
		t.Fatalf("expected 2 queries in pair, got %d", len(got[0].Queries))
	}
}

func TestDetectJoinPairs_NoJoins(t *testing.T) {
	queries := []QueryInfo{
		{QueryID: 1, Text: "SELECT * FROM users WHERE id = 1"},
	}
	got := DetectJoinPairs(queries)
	if len(got) != 0 {
		t.Fatalf("expected 0 pairs for no-join query, got %d", len(got))
	}
}

// ---------------------------------------------------------------------------
// DetectMatViewCandidates
// ---------------------------------------------------------------------------

func TestDetectMatViewCandidates_SeqScanGroupBy(t *testing.T) {
	queries := []QueryInfo{
		{QueryID: 1, Text: "SELECT status, count(*) FROM t GROUP BY status"},
	}
	plans := []PlanSummary{
		{QueryID: 1, ScanType: "Seq Scan"},
	}
	got := DetectMatViewCandidates(queries, plans)
	if len(got) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(got))
	}
	if got[0] != 1 {
		t.Fatalf("expected QueryID=1, got %d", got[0])
	}
}

func TestDetectMatViewCandidates_IndexScanGroupBy(t *testing.T) {
	queries := []QueryInfo{
		{QueryID: 1, Text: "SELECT status, count(*) FROM t GROUP BY status"},
	}
	plans := []PlanSummary{
		{QueryID: 1, ScanType: "Index Scan"},
	}
	got := DetectMatViewCandidates(queries, plans)
	if len(got) != 0 {
		t.Fatalf(
			"expected 0 candidates for Index Scan + GROUP BY, got %d",
			len(got),
		)
	}
}

func TestDetectMatViewCandidates_SeqScanNoGroupBy(t *testing.T) {
	queries := []QueryInfo{
		{QueryID: 1, Text: "SELECT * FROM t WHERE id = 1"},
	}
	plans := []PlanSummary{
		{QueryID: 1, ScanType: "Seq Scan"},
	}
	got := DetectMatViewCandidates(queries, plans)
	if len(got) != 0 {
		t.Fatalf(
			"expected 0 candidates for Seq Scan without GROUP BY, got %d",
			len(got),
		)
	}
}

// ---------------------------------------------------------------------------
// DetectParamTuningNeeds
// ---------------------------------------------------------------------------

func TestDetectParamTuningNeeds_SortDisk(t *testing.T) {
	plans := []PlanSummary{
		{QueryID: 1, SortDisk: 1024},
	}
	got := DetectParamTuningNeeds(plans)
	if _, ok := got["work_mem_sort"]; !ok {
		t.Fatal("expected work_mem_sort recommendation for SortDisk > 0")
	}
}

func TestDetectParamTuningNeeds_HashBatches(t *testing.T) {
	plans := []PlanSummary{
		{QueryID: 1, Summary: "Hash Batches: 16"},
	}
	got := DetectParamTuningNeeds(plans)
	if _, ok := got["work_mem_hash"]; !ok {
		t.Fatal("expected work_mem_hash recommendation for Hash Batches")
	}
}

func TestDetectParamTuningNeeds_CleanPlan(t *testing.T) {
	plans := []PlanSummary{
		{QueryID: 1, ScanType: "Index Scan", Summary: "no issues"},
	}
	got := DetectParamTuningNeeds(plans)
	if len(got) != 0 {
		t.Fatalf("expected 0 recommendations for clean plan, got %d", len(got))
	}
}

// ---------------------------------------------------------------------------
// DetectBloatedIndexes
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// IsBRINCandidate
// ---------------------------------------------------------------------------

func TestIsBRINCandidate(t *testing.T) {
	tests := []struct {
		name     string
		stats    []ColStat
		column   string
		expected bool
	}{
		{
			name:     "high positive correlation",
			stats:    []ColStat{{Column: "created_at", Correlation: 0.95}},
			column:   "created_at",
			expected: true,
		},
		{
			name:     "high negative correlation",
			stats:    []ColStat{{Column: "id", Correlation: -0.9}},
			column:   "id",
			expected: true,
		},
		{
			name:     "low correlation",
			stats:    []ColStat{{Column: "email", Correlation: 0.5}},
			column:   "email",
			expected: false,
		},
		{
			name:     "column not in stats",
			stats:    []ColStat{{Column: "other", Correlation: 0.99}},
			column:   "missing",
			expected: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsBRINCandidate(tt.stats, tt.column)
			if got != tt.expected {
				t.Fatalf("IsBRINCandidate(%q) = %v, want %v",
					tt.column, got, tt.expected)
			}
		})
	}
}
