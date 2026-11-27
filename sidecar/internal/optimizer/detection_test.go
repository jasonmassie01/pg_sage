package optimizer

import "testing"

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
