package briefing

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// Dogfood round 2 (lifeos 2026-10-04): the briefing told the operator to
// "investigate" a slow_query finding whose statement was an untagged
// EXPLAIN ANALYZE of a pg_sage probe. A finding about a diagnostic
// statement opened before the analyzer rule existed must not reach the
// LLM input, nor count as open work, until the analyzer resolves it.
func TestGatherFindingsLeavesOutDiagnosticStatements(t *testing.T) {
	pool, ctx := requireDB(t)
	if _, err := pool.Exec(ctx, `DELETE FROM sage.findings WHERE category IN
		('slow_query', 'plan_regression')`); err != nil {
		t.Fatalf("clean findings: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.findings
			WHERE object_identifier LIKE 'queryid:9900%'`)
	})
	seed := []struct{ ident, key, query string }{
		{"queryid:99001", "query", "EXPLAIN (ANALYZE, BUFFERS, SUMMARY ON) WITH s AS " +
			"(SELECT 1) SELECT * FROM s"},
		{"queryid:99002", "query_text", "VACUUM public.orders"},
		{"queryid:99003", "query", "SELECT * FROM orders WHERE id = $1"},
	}
	for _, s := range seed {
		_, err := pool.Exec(ctx, `INSERT INTO sage.findings (category, severity,
			object_type, object_identifier, title, detail, status, last_seen)
			VALUES ('slow_query', 'critical', 'query', $1, 'Slow query '||$1,
			        jsonb_build_object($2::text, $3::text), 'open', now())`,
			s.ident, s.key, s.query)
		if err != nil {
			t.Fatalf("seed %s: %v", s.ident, err)
		}
	}
	w := New(pool, &config.Config{}, nil, noopLog)
	raw, total, err := w.gatherFindings(ctx)
	if err != nil {
		t.Fatalf("gatherFindings: %v", err)
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatalf("decode findings %q: %v", raw, err)
	}
	if strings.Contains(raw, "queryid:99001") || strings.Contains(raw, "queryid:99002") {
		t.Fatalf("briefing input names a diagnostic statement: %s", raw)
	}
	if !strings.Contains(raw, "queryid:99003") {
		t.Fatalf("briefing input lost the application finding: %s", raw)
	}
	if len(list) < maxBriefingFindings && total != len(list) {
		t.Fatalf("open count %d, listed %d: the count must use the same rule", total,
			len(list))
	}
}
