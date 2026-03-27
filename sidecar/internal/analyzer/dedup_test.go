package analyzer

import "testing"

func noopLog(string, string, ...any) {}

func TestDedupFindings_Empty(t *testing.T) {
	got := DedupFindings(nil, noopLog)
	if got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestDedupFindings_Single(t *testing.T) {
	in := []Finding{{
		Category:         "slow_query",
		Severity:         "warning",
		ObjectIdentifier: "q:123",
	}}
	got := DedupFindings(in, noopLog)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
}

func TestDedupFindings_NoDuplicates(t *testing.T) {
	in := []Finding{
		{
			Category: "slow_query", Severity: "warning",
			ObjectIdentifier: "q:1",
		},
		{
			Category: "unused_index", Severity: "info",
			ObjectIdentifier: "idx:2",
		},
		{
			Category: "table_bloat", Severity: "critical",
			ObjectIdentifier: "t:3",
		},
	}
	got := DedupFindings(in, noopLog)
	if len(got) != 3 {
		t.Fatalf("expected 3 findings, got %d", len(got))
	}
}

func TestDedupFindings_SameObjectSameCategory(t *testing.T) {
	in := []Finding{
		{
			Category: "slow_query", Severity: "warning",
			ObjectIdentifier: "q:1", Title: "low",
		},
		{
			Category: "slow_query", Severity: "critical",
			ObjectIdentifier: "q:1", Title: "high",
		},
	}
	got := DedupFindings(in, noopLog)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	if got[0].Severity != "critical" {
		t.Errorf("expected critical, got %s", got[0].Severity)
	}
}

func TestDedupFindings_SameObjectDiffCategory(t *testing.T) {
	in := []Finding{
		{
			Category: "slow_query", Severity: "warning",
			ObjectIdentifier: "q:1",
		},
		{
			Category: "seq_scan_heavy", Severity: "info",
			ObjectIdentifier: "q:1",
		},
	}
	got := DedupFindings(in, noopLog)
	if len(got) != 2 {
		t.Fatalf(
			"expected 2 (different categories), got %d",
			len(got),
		)
	}
}

func TestDedupFindings_QueryTuningBeatsGlobal(t *testing.T) {
	in := []Finding{
		{
			Category:         "memory_tuning",
			Severity:         "warning",
			ObjectIdentifier: "q:42",
			Title:            "reduce work_mem globally",
		},
		{
			Category:         "query_tuning",
			Severity:         "info",
			ObjectIdentifier: "q:42",
			Title:            "increase work_mem for query",
			RecommendedSQL:   "SET work_mem = '256MB'",
		},
	}
	got := DedupFindings(in, noopLog)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	if got[0].Category != "query_tuning" {
		t.Errorf(
			"expected query_tuning to win, got %s",
			got[0].Category,
		)
	}
}

func TestDedupFindings_SameSeverityPrefersSQL(t *testing.T) {
	in := []Finding{
		{
			Category: "slow_query", Severity: "warning",
			ObjectIdentifier: "q:5", Title: "no sql",
		},
		{
			Category: "slow_query", Severity: "warning",
			ObjectIdentifier: "q:5", Title: "has sql",
			RecommendedSQL: "CREATE INDEX ...",
		},
	}
	got := DedupFindings(in, noopLog)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	if got[0].RecommendedSQL == "" {
		t.Error("expected finding with RecommendedSQL")
	}
}

func TestDedupFindings_ThreeSameTwoSameCategory(t *testing.T) {
	in := []Finding{
		{
			Category: "slow_query", Severity: "info",
			ObjectIdentifier: "q:9", Title: "a",
		},
		{
			Category: "slow_query", Severity: "warning",
			ObjectIdentifier: "q:9", Title: "b",
		},
		{
			Category: "seq_scan_heavy", Severity: "info",
			ObjectIdentifier: "q:9", Title: "c",
		},
	}
	got := DedupFindings(in, noopLog)
	if len(got) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(got))
	}
	cats := map[string]bool{}
	for _, f := range got {
		cats[f.Category] = true
	}
	if !cats["slow_query"] || !cats["seq_scan_heavy"] {
		t.Errorf("unexpected categories: %v", cats)
	}
	for _, f := range got {
		if f.Category == "slow_query" && f.Severity != "warning" {
			t.Errorf(
				"slow_query should be warning, got %s",
				f.Severity,
			)
		}
	}
}

func TestDedupFindings_VacuumTuningBeatenByQueryTuning(
	t *testing.T,
) {
	in := []Finding{
		{
			Category:         "vacuum_tuning",
			Severity:         "warning",
			ObjectIdentifier: "q:7",
		},
		{
			Category:         "query_tuning",
			Severity:         "info",
			ObjectIdentifier: "q:7",
		},
	}
	got := DedupFindings(in, noopLog)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	if got[0].Category != "query_tuning" {
		t.Errorf("expected query_tuning, got %s", got[0].Category)
	}
}

func TestDedupFindings_NonTuningCategoryKept(t *testing.T) {
	// Categories that don't end in _tuning should not be
	// removed by the query_tuning rule.
	in := []Finding{
		{
			Category:         "slow_query",
			Severity:         "warning",
			ObjectIdentifier: "q:10",
		},
		{
			Category:         "query_tuning",
			Severity:         "info",
			ObjectIdentifier: "q:10",
		},
	}
	got := DedupFindings(in, noopLog)
	if len(got) != 2 {
		t.Fatalf(
			"expected 2 (slow_query not global config), got %d",
			len(got),
		)
	}
}
