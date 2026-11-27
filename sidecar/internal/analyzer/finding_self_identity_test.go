package analyzer

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/selfcost"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

// footprint-06: the self categories describe pg_sage's cost on a database
// and are keyed by the database name. A monitored (or meta) database named
// sage must keep them, and their text may name pg_sage.
// No concurrent access tests: the classifiers are pure functions and the
// upsert's concurrency is covered by the finding upsert tests.

func sageDatabaseSelfFindings() []Finding {
	cost := knownCost(9000)
	cost.Database = "sage"
	fp := footprint(5*floor, 5*floor)
	fp.database = "sage"
	top := append(sampleTop(), selfcost.StatementCost{
		Text: "/* pg_sage */ SELECT * FROM pg_stat_statements", TimeMs: 1, Calls: 1})
	out := ruleSelfCost(cost, 3000)
	out = append(out, ruleSageFootprint(fp, 10*floor, 10)...)
	return append(out, ruleSelfBudget("sage", sampleBreaches(), top, sampleLoops())...)
}

func cleanSelfIdentityFindings(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `DELETE FROM sage.findings
		WHERE category IN ('sage_self_cost', 'sage_footprint', 'sage_self_budget')
		   OR category LIKE 'test_phase2_selfid%'`)
	if err != nil {
		t.Fatalf("clean findings: %v", err)
	}
}

func TestSelfFindings_TitlesNamePgSage(t *testing.T) {
	got := sageDatabaseSelfFindings()
	if len(got) != 3 {
		t.Fatalf("self findings = %d, want 3 (cost, footprint, budget)", len(got))
	}
	for _, f := range got {
		if !strings.Contains(f.Title, "pg_sage") {
			t.Errorf("%s title %q does not name pg_sage", f.Category, f.Title)
		}
		if isSelfMonitoringFinding(f) || excludedFromAdvice(f) {
			t.Errorf("%s on database sage would be dropped: %+v", f.Category, f)
		}
	}
}

// The categories exempted by selfmonitor are the analyzer's own.
func TestSelfFindingCategories_MatchAnalyzerRules(t *testing.T) {
	want := map[string]bool{categorySelfCost: true, categorySelfBudget: true,
		categorySageFootprint: true}
	got := map[string]bool{}
	for _, c := range selfmonitor.SelfFindingCategories {
		got[c] = true
	}
	for c := range want {
		if !got[c] {
			t.Errorf("selfmonitor does not exempt the analyzer's %q", c)
		}
	}
	if len(got) != len(want) {
		t.Errorf("selfmonitor exempts %v, analyzer raises %v", got, want)
	}
}

func TestUpsertFindings_SelfCategoriesPersistForDatabaseSage(t *testing.T) {
	pool := phase2Pool(t)
	cleanSelfIdentityFindings(t, pool)
	t.Cleanup(func() { cleanSelfIdentityFindings(t, pool) })
	ctx := context.Background()
	if err := UpsertFindings(ctx, pool, sageDatabaseSelfFindings()); err != nil {
		t.Fatalf("UpsertFindings: %v", err)
	}
	rows, err := pool.Query(ctx, `SELECT category, title FROM sage.findings
		WHERE object_identifier = 'sage' AND status = 'open' ORDER BY category`)
	if err != nil {
		t.Fatalf("read findings: %v", err)
	}
	defer rows.Close()
	var cats []string
	for rows.Next() {
		var cat, title string
		if err := rows.Scan(&cat, &title); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(title, "pg_sage") {
			t.Errorf("%s persisted title %q does not name pg_sage", cat, title)
		}
		cats = append(cats, cat)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(cats, ",") != "sage_footprint,sage_self_budget,sage_self_cost" {
		t.Fatalf("persisted categories for database sage = %v, want all three", cats)
	}
}

// The identifier rule keeps user objects named sage and still drops
// pg_sage's own schema-qualified objects.
func TestUpsertFindings_SelfIdentifierRules(t *testing.T) {
	pool := phase2Pool(t)
	cleanSelfIdentityFindings(t, pool)
	t.Cleanup(func() { cleanSelfIdentityFindings(t, pool) })
	ctx := context.Background()
	in := []Finding{
		{Category: "test_phase2_selfid_index", Severity: "info", ObjectType: "index",
			ObjectIdentifier: "sage.snapshots_pkey", Title: "Unused index",
			Detail: map[string]any{}},
		{Category: "test_phase2_selfid_table", Severity: "info", ObjectType: "table",
			ObjectIdentifier: "sage", Title: "Table bloat", Detail: map[string]any{}},
		{Category: "test_phase2_selfid_events", Severity: "info", ObjectType: "table",
			ObjectIdentifier: "public.pg_sage_events", Title: "Table bloat",
			Detail: map[string]any{}},
		{Category: "test_phase2_selfid_monitor", Severity: "warning", ObjectType: "query",
			ObjectIdentifier: "queryid:77", Title: "Slow query",
			Detail: map[string]any{"query": "SELECT * FROM pg_stat_statements"}},
	}
	if err := UpsertFindings(ctx, pool, in); err != nil {
		t.Fatalf("UpsertFindings: %v", err)
	}
	var kept []string
	rows, err := pool.Query(ctx, `SELECT category FROM sage.findings
		WHERE category LIKE 'test_phase2_selfid%' ORDER BY category`)
	if err != nil {
		t.Fatalf("read findings: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		kept = append(kept, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := "test_phase2_selfid_events,test_phase2_selfid_monitor,test_phase2_selfid_table"
	if strings.Join(kept, ",") != want {
		t.Fatalf("kept %v, want %s (the sage.snapshots_pkey index dropped)", kept, want)
	}
}
