package schema

import (
	"strings"
	"testing"
)

// perf v1.8.3 (claude/perf-api): the findings and actions lists page by
// keyset over an index matching each list's filter and order, and the
// actions list counts attempts per page row through an index on the SQL.
var apiListIndexWant = map[string][]string{
	"idx_findings_list_severity": {"(status, (", "CASE severity",
		"WHEN 'critical'::text THEN 3", "WHEN 'warning'::text THEN 2",
		"WHEN 'info'::text THEN 1", "ELSE 0", "last_seen, id)"},
	"idx_findings_list_last_seen": {"(status, last_seen, id)"},
	"idx_action_log_time_id":      {"(executed_at, id)"},
	"idx_action_log_sql_md5":      {"(md5(sql_executed), executed_at)"},
	"idx_action_queue_ledger": {"(proposed_at, id) WHERE ((status <> 'executed'::text) " +
		"AND (proposed_at IS NOT NULL))"},
}

func TestAPIListIndexMigration_CreatesIdempotentIndexes(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	for name, parts := range apiListIndexWant {
		var def string
		err := pool.QueryRow(ctx, `SELECT pg_get_indexdef(to_regclass('sage.' || $1))`,
			name).Scan(&def)
		if err != nil || def == "" {
			t.Errorf("index %s missing: %v", name, err)
			continue
		}
		for _, part := range parts {
			if !strings.Contains(def, part) {
				t.Errorf("index %s = %s, missing %s", name, def, part)
			}
		}
	}
}

// A dropped or invalid index is rebuilt by the next bootstrap; an existing
// valid one is left alone (no lock-taking CREATE on every start).
func TestAPIListIndexMigration_RebuildsDroppedIndex(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	var oidBefore uint32
	if err := pool.QueryRow(ctx, `SELECT 'sage.idx_findings_list_last_seen'::regclass::oid`).
		Scan(&oidBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "DROP INDEX sage.idx_action_log_sql_md5"); err != nil {
		t.Fatal(err)
	}
	bootstrapWithRetry(t, ctx, pool)
	var exists bool
	var oidAfter uint32
	if err := pool.QueryRow(ctx, `SELECT to_regclass('sage.idx_action_log_sql_md5') IS NOT NULL,
		'sage.idx_findings_list_last_seen'::regclass::oid`).Scan(&exists, &oidAfter); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("dropped index not rebuilt")
	}
	if oidAfter != oidBefore {
		t.Fatal("an existing valid index was rebuilt")
	}
}

// No index here duplicates one another migration already creates.
func TestAPIListIndexMigration_NoDuplicateDefinitions(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	rows, err := pool.Query(ctx, `SELECT i.indexrelid::regclass::text, count(*) OVER (
			PARTITION BY i.indrelid, i.indkey::text, pg_get_expr(i.indexprs, i.indrelid),
			             pg_get_expr(i.indpred, i.indrelid))
		  FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		 WHERE c.relnamespace = 'sage'::regnamespace`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			t.Fatal(err)
		}
		short := strings.TrimPrefix(name, "sage.")
		if _, mine := apiListIndexWant[short]; mine && n > 1 {
			t.Errorf("%s duplicates another sage index", name)
		}
	}
}
