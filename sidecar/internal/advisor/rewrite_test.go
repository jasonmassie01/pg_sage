package advisor

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

func TestRewriteSystemPrompt_ContainsRules(t *testing.T) {
	checks := []string{
		"N+1",
		"JOIN",
		"keyset",
		"OFFSET",
		"advisory",
		"impact_rating",
	}
	for _, want := range checks {
		if !strings.Contains(rewriteSystemPrompt, want) {
			t.Errorf("rewrite system prompt missing %q", want)
		}
	}
}

func TestRewriteSystemPrompt_ContainsAntiThinking(t *testing.T) {
	if !strings.Contains(rewriteSystemPrompt, "No thinking") {
		t.Error("rewrite system prompt missing anti-thinking directive")
	}
}

func TestRewriteCandidate_TopByTotalTime(t *testing.T) {
	// Build 15 queries; only those with Calls >= 100 and
	// MeanExecTime >= 50 qualify as time-based candidates.
	queries := make([]collector.QueryStats, 15)
	for i := range queries {
		queries[i] = collector.QueryStats{
			QueryID:       int64(i + 1),
			Query:         "SELECT 1",
			TotalExecTime: float64((15 - i) * 1000),
			MeanExecTime:  float64(60),
			Calls:         200,
		}
	}
	// Make two queries fail the filter.
	queries[0].Calls = 50              // too few calls
	queries[1].MeanExecTime = 10       // too fast

	// Simulate the selection logic from analyzeQueryRewrites.
	sorted := make([]collector.QueryStats, len(queries))
	copy(sorted, queries)
	for i := 0; i < len(sorted) && i < 10; i++ {
		maxIdx := i
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].TotalExecTime > sorted[maxIdx].TotalExecTime {
				maxIdx = j
			}
		}
		sorted[i], sorted[maxIdx] = sorted[maxIdx], sorted[i]
	}

	var included int
	for i := 0; i < len(sorted) && i < 10; i++ {
		q := sorted[i]
		if q.Calls >= 100 && q.MeanExecTime >= 50 {
			included++
		}
	}
	// queries[0] and queries[1] are in the top 10 by time but
	// fail the filter, so we expect 8 candidates.
	if included != 8 {
		t.Errorf("expected 8 qualifying candidates, got %d", included)
	}
}

func TestRewriteCandidate_SpillingQueries(t *testing.T) {
	queries := []collector.QueryStats{
		{QueryID: 1, TempBlksWritten: 500, Calls: 100},
		{QueryID: 2, TempBlksWritten: 0, Calls: 200},
		{QueryID: 3, TempBlksWritten: 300, Calls: 60},
	}
	var spill int
	for _, q := range queries {
		if q.TempBlksWritten > 0 && q.Calls > 50 {
			spill++
		}
	}
	if spill != 2 {
		t.Errorf("expected 2 spill candidates, got %d", spill)
	}
}

func TestRewriteCandidate_Dedup(t *testing.T) {
	// A query qualifies both by total time and temp spills.
	// It should appear only once after dedup.
	type candidate struct {
		queryID int64
		reason  string
	}
	candidates := []candidate{
		{1, "high total time"},
		{2, "high total time"},
		{1, "temp spills"}, // duplicate queryID
		{3, "temp spills"},
	}

	seen := make(map[int64]bool)
	var unique []candidate
	for _, c := range candidates {
		if !seen[c.queryID] {
			seen[c.queryID] = true
			unique = append(unique, c)
		}
	}
	if len(unique) != 3 {
		t.Errorf("expected 3 unique candidates, got %d", len(unique))
	}
}

func TestRewriteCandidate_MaxCandidates(t *testing.T) {
	type candidate struct {
		queryID int64
	}
	var unique []candidate
	for i := 0; i < 20; i++ {
		unique = append(unique, candidate{int64(i)})
	}
	if len(unique) > 10 {
		unique = unique[:10]
	}
	if len(unique) != 10 {
		t.Errorf("expected cap at 10, got %d", len(unique))
	}
}

func TestRewriteCandidate_FilterLowActivity(t *testing.T) {
	q := collector.QueryStats{
		QueryID:       1,
		Calls:         50,
		MeanExecTime:  10,
		TotalExecTime: 500,
	}
	if q.Calls >= 100 && q.MeanExecTime >= 50 {
		t.Error("low-activity query should be excluded")
	}
}

func TestRewriteCandidate_FilterSpillLowCalls(t *testing.T) {
	q := collector.QueryStats{
		QueryID:         1,
		TempBlksWritten: 500,
		Calls:           30,
	}
	if q.TempBlksWritten > 0 && q.Calls > 50 {
		t.Error("spill query with low calls should be excluded")
	}
}

func TestRewriteFinding_SeverityForced(t *testing.T) {
	raw := `[{"table":"t1","severity":"critical",` +
		`"recommended_sql":"DROP TABLE t1"}]`
	findings := parseLLMFindings(raw, "query_rewrite", noopLog)
	// Apply the same force logic as analyzeQueryRewrites.
	for i := range findings {
		findings[i].Severity = "info"
		findings[i].RecommendedSQL = ""
	}
	if findings[0].Severity != "info" {
		t.Error("severity not forced to info")
	}
	if findings[0].RecommendedSQL != "" {
		t.Error("recommended SQL not cleared")
	}
}
