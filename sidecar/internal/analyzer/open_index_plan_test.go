package analyzer

import (
	"context"
	"strings"
	"testing"
)

// Performance gate: the analyzer reads the open index findings every
// cycle. It must read them through the open-findings partial index, not
// by scanning every finding ever recorded (reviews/2026-10-03-perf-gate-
// report.md), and suppressed findings stay excluded.
func TestOpenIndexRecommendationTables_OpenOnlyThroughPartialIndex(t *testing.T) {
	pool := phase2Pool(t)
	ctx := context.Background()
	a := New(pool, phase2Config(), nil, nil, nil, nil, nil, noopLog)
	cleanup := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.findings WHERE title LIKE 'perfgate_open_%'`)
	}
	cleanup()
	t.Cleanup(cleanup)
	for _, r := range []struct{ ident, status string }{
		{"public.open_one", "open"}, {"public.suppressed_one", "suppressed"},
		{"public.resolved_one", "resolved"},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO sage.findings (category, severity,
			object_type, object_identifier, title, detail, status)
			VALUES ('missing_index', 'warning', 'index', $1, 'perfgate_open_' || $1,
			'{}'::jsonb, $2)`, r.ident, r.status); err != nil {
			t.Fatalf("insert %s: %v", r.ident, err)
		}
	}
	got := strings.Join(a.openIndexRecommendationTables(ctx), ",")
	if !strings.Contains(got, "public.open_one") || strings.Contains(got, "suppressed_one") ||
		strings.Contains(got, "resolved_one") {
		t.Fatalf("open index tables = %s", got)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	rows, err := conn.Query(ctx, "EXPLAIN "+openIndexFindingsSQL)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	rows.Close()
	if strings.Contains(plan.String(), "Seq Scan on findings") {
		t.Fatalf("open index findings scan sage.findings sequentially:\n%s", plan.String())
	}
}
