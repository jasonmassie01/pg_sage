package changefeed

import (
	"context"
	"strings"
	"testing"
)

// The migration detector's feed reads one category of findings past a
// cursor. Without an index on it the read was a parallel scan of all of
// sage.findings every poll (perf gate: 150,000 rows, twice per steady
// phase); the partial index makes it a short index range scan.
func TestMigrationFeedReadsThroughItsIndex(t *testing.T) {
	pool, ctx := livePool(t)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, status, created_at, last_seen)
		SELECT 'feedplan_other', 'info', 'table', 'feedplan.t_' || g, 'noise', '{}',
		       'resolved', now(), now()
		FROM generate_series(1, 20000) g`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.findings WHERE category = 'feedplan_other'")
	})
	if _, err := pool.Exec(ctx, "ANALYZE sage.findings"); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, "EXPLAIN "+migrationFeedSQL, int64(0), 100)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	var lines []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, l)
	}
	rows.Close()
	text := strings.Join(lines, "\n")
	if strings.Contains(text, "Seq Scan on findings") ||
		!strings.Contains(text, "idx_findings_migration_feed") {
		t.Fatalf("migration feed does not read through its index:\n%s", text)
	}
}
