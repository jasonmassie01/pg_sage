package analyzer

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

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

func TestDedupFindings_HintCategoryBeatsGlobal(t *testing.T) {
	in := []Finding{
		{
			Category:         "memory_tuning",
			Severity:         "warning",
			ObjectIdentifier: "q:50",
			Title:            "reduce work_mem globally",
		},
		{
			Category:         "work_mem_hint",
			Severity:         "info",
			ObjectIdentifier: "q:50",
			Title:            "increase work_mem for query",
			RecommendedSQL:   "SET work_mem = '512MB'",
		},
	}
	got := DedupFindings(in, noopLog)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	if got[0].Category != "work_mem_hint" {
		t.Errorf(
			"expected work_mem_hint to win, got %s",
			got[0].Category,
		)
	}
}

func TestDedupFindings_TunerCategoryBeatsGlobal(t *testing.T) {
	in := []Finding{
		{
			Category:         "memory_tuning",
			Severity:         "critical",
			ObjectIdentifier: "q:60",
			Title:            "lower work_mem",
		},
		{
			Category:         "query_tuner",
			Severity:         "info",
			ObjectIdentifier: "q:60",
			Title:            "raise work_mem for this query",
		},
	}
	got := DedupFindings(in, noopLog)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	if got[0].Category != "query_tuner" {
		t.Errorf(
			"expected query_tuner to win, got %s",
			got[0].Category,
		)
	}
}

func TestDeduplicateFindings_VacuumDowngradeHighIO(
	t *testing.T,
) {
	in := []Finding{
		{
			Category:         "vacuum_tuning",
			Severity:         "warning",
			ObjectIdentifier: "t:orders",
			Title:            "lower vacuum_cost_delay",
		},
		{
			Category:         "slow_query",
			Severity:         "critical",
			ObjectIdentifier: "q:99",
			Title:            "slow select",
		},
	}
	got := DeduplicateFindings(in, 55.0, noopLog)
	if len(got) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(got))
	}
	for _, f := range got {
		if f.Category == "vacuum_tuning" && f.Severity != "info" {
			t.Errorf(
				"vacuum finding should be info at 55%% IO, got %s",
				f.Severity,
			)
		}
		if f.Category == "slow_query" && f.Severity != "critical" {
			t.Errorf(
				"non-vacuum finding should be unchanged, got %s",
				f.Severity,
			)
		}
	}
}

func TestDeduplicateFindings_VacuumNoDowngradeLowIO(
	t *testing.T,
) {
	in := []Finding{
		{
			Category:         "vacuum_tuning",
			Severity:         "warning",
			ObjectIdentifier: "t:orders",
			Title:            "lower vacuum_cost_delay",
		},
	}
	got := DeduplicateFindings(in, 49.0, noopLog)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	if got[0].Severity != "warning" {
		t.Errorf(
			"vacuum finding should stay warning at 49%% IO, got %s",
			got[0].Severity,
		)
	}
}

func TestDeduplicateFindings_VacuumNoDowngradeAtExactly50(
	t *testing.T,
) {
	in := []Finding{
		{
			Category:         "vacuum_tuning",
			Severity:         "warning",
			ObjectIdentifier: "t:orders",
			Title:            "lower vacuum_cost_delay",
		},
	}
	got := DeduplicateFindings(in, 50.0, noopLog)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	if got[0].Severity != "warning" {
		t.Errorf(
			"vacuum finding should stay warning at exactly 50%%, got %s",
			got[0].Severity,
		)
	}
}

func TestDeduplicateFindings_EmptySlice(t *testing.T) {
	got := DeduplicateFindings([]Finding{}, 90.0, noopLog)
	if len(got) != 0 {
		t.Fatalf("expected empty, got %d", len(got))
	}
}

func TestDedupFindings_ConflictingSQLKeepsHigherSeverity(
	t *testing.T,
) {
	in := []Finding{
		{
			Category:         "index_recommendation",
			Severity:         "info",
			ObjectIdentifier: "public.orders",
			Title:            "add partial index",
			RecommendedSQL:   "CREATE INDEX idx_a ON orders(id) WHERE status='active'",
		},
		{
			Category:         "index_recommendation",
			Severity:         "warning",
			ObjectIdentifier: "public.orders",
			Title:            "add full index",
			RecommendedSQL:   "CREATE INDEX idx_b ON orders(id)",
		},
	}
	got := DedupFindings(in, noopLog)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	if got[0].Severity != "warning" {
		t.Errorf("expected warning (higher), got %s", got[0].Severity)
	}
	if got[0].Title != "add full index" {
		t.Errorf("expected 'add full index', got %s", got[0].Title)
	}
}

func TestComputeIOUtilPct_NilSnapshot(t *testing.T) {
	got := computeIOUtilPct(nil)
	if got != 0 {
		t.Errorf("expected 0 for nil snapshot, got %f", got)
	}
}

func TestComputeIOUtilPct_NoIOWait(t *testing.T) {
	snap := &collector.Snapshot{
		System: collector.SystemStats{
			BlkReadTime:  0,
			BlkWriteTime: 0,
		},
		Queries: []collector.QueryStats{
			{TotalExecTime: 1000},
		},
	}
	got := computeIOUtilPct(snap)
	if got != 0 {
		t.Errorf("expected 0 with no IO wait, got %f", got)
	}
}

func TestComputeIOUtilPct_NoQueries(t *testing.T) {
	snap := &collector.Snapshot{
		System: collector.SystemStats{
			BlkReadTime:  500,
			BlkWriteTime: 200,
		},
	}
	got := computeIOUtilPct(snap)
	if got != 0 {
		t.Errorf("expected 0 with no queries, got %f", got)
	}
}

func TestComputeIOUtilPct_HighIOWait(t *testing.T) {
	snap := &collector.Snapshot{
		System: collector.SystemStats{
			BlkReadTime:  600,
			BlkWriteTime: 200,
		},
		Queries: []collector.QueryStats{
			{TotalExecTime: 1000},
		},
	}
	got := computeIOUtilPct(snap)
	// (600+200)/1000 * 100 = 80%
	if got < 79.9 || got > 80.1 {
		t.Errorf("expected ~80%%, got %f", got)
	}
}

func TestComputeIOUtilPct_CappedAt100(t *testing.T) {
	// blk_read_time can exceed total_exec_time because they
	// measure different things (cumulative vs wall clock).
	snap := &collector.Snapshot{
		System: collector.SystemStats{
			BlkReadTime:  2000,
			BlkWriteTime: 500,
		},
		Queries: []collector.QueryStats{
			{TotalExecTime: 1000},
		},
	}
	got := computeIOUtilPct(snap)
	if got != 100 {
		t.Errorf("expected capped at 100, got %f", got)
	}
}

func TestComputeIOUtilPct_MultipleQueries(t *testing.T) {
	snap := &collector.Snapshot{
		System: collector.SystemStats{
			BlkReadTime:  300,
			BlkWriteTime: 200,
		},
		Queries: []collector.QueryStats{
			{TotalExecTime: 500},
			{TotalExecTime: 500},
		},
	}
	got := computeIOUtilPct(snap)
	// (300+200)/(500+500) * 100 = 50%
	if got < 49.9 || got > 50.1 {
		t.Errorf("expected ~50%%, got %f", got)
	}
}

func TestExtractGUCTarget_AlterSystem(t *testing.T) {
	guc, pt := extractGUCTarget(
		"ALTER SYSTEM SET work_mem = '256MB'",
	)
	if guc != "work_mem" {
		t.Errorf("expected work_mem, got %q", guc)
	}
	if pt {
		t.Error("expected perTable=false for ALTER SYSTEM")
	}
}

func TestExtractGUCTarget_AlterTable(t *testing.T) {
	guc, pt := extractGUCTarget(
		"ALTER TABLE orders SET (autovacuum_vacuum_cost_delay = 10)",
	)
	if guc != "autovacuum_vacuum_cost_delay" {
		t.Errorf(
			"expected autovacuum_vacuum_cost_delay, got %q", guc,
		)
	}
	if !pt {
		t.Error("expected perTable=true for ALTER TABLE")
	}
}

func TestExtractGUCTarget_Set(t *testing.T) {
	guc, pt := extractGUCTarget("SET work_mem = '128MB'")
	if guc != "work_mem" {
		t.Errorf("expected work_mem, got %q", guc)
	}
	if pt {
		t.Error("expected perTable=false for SET")
	}
}

func TestExtractGUCTarget_NoGUC(t *testing.T) {
	guc, pt := extractGUCTarget(
		"CREATE INDEX idx_foo ON bar(baz)",
	)
	if guc != "" {
		t.Errorf("expected empty guc, got %q", guc)
	}
	if pt {
		t.Error("expected perTable=false for non-GUC")
	}
}

func TestDedupFindings_ConflictingGUC_HigherSeverityWins(
	t *testing.T,
) {
	in := []Finding{
		{
			Category:         "vacuum_tuning",
			Severity:         "warning",
			ObjectIdentifier: "global",
			Title:            "set work_mem 32MB",
			RecommendedSQL:   "ALTER SYSTEM SET work_mem = '32MB'",
		},
		{
			Category:         "memory_tuning",
			Severity:         "critical",
			ObjectIdentifier: "global",
			Title:            "set work_mem 256MB",
			RecommendedSQL:   "ALTER SYSTEM SET work_mem = '256MB'",
		},
	}
	got := DedupFindings(in, noopLog)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	if got[0].Severity != "critical" {
		t.Errorf("expected critical to win, got %s", got[0].Severity)
	}
	if got[0].Title != "set work_mem 256MB" {
		t.Errorf("expected 256MB winner, got %s", got[0].Title)
	}
}

func TestDedupFindings_ConflictingGUC_PerTableBeatsGlobal(
	t *testing.T,
) {
	in := []Finding{
		{
			Category:         "memory_tuning",
			Severity:         "critical",
			ObjectIdentifier: "global",
			Title:            "global work_mem",
			RecommendedSQL: "ALTER SYSTEM SET " +
				"autovacuum_vacuum_cost_delay = 20",
		},
		{
			Category:         "vacuum_tuning",
			Severity:         "warning",
			ObjectIdentifier: "public.orders",
			Title:            "per-table cost_delay",
			RecommendedSQL: "ALTER TABLE orders SET " +
				"(autovacuum_vacuum_cost_delay = 10)",
		},
	}
	got := DedupFindings(in, noopLog)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	if got[0].Title != "per-table cost_delay" {
		t.Errorf(
			"expected per-table to win, got %s", got[0].Title,
		)
	}
}

func TestDedupFindings_NoGUCConflict(t *testing.T) {
	in := []Finding{
		{
			Category:         "memory_tuning",
			Severity:         "warning",
			ObjectIdentifier: "guc:work_mem",
			Title:            "set work_mem",
			RecommendedSQL:   "ALTER SYSTEM SET work_mem = '256MB'",
		},
		{
			Category:         "memory_tuning",
			Severity:         "warning",
			ObjectIdentifier: "guc:shared_buffers",
			Title:            "set shared_buffers",
			RecommendedSQL: "ALTER SYSTEM SET " +
				"shared_buffers = '2GB'",
		},
	}
	got := DedupFindings(in, noopLog)
	if len(got) != 2 {
		t.Fatalf(
			"expected 2 (different GUCs), got %d", len(got),
		)
	}
}
