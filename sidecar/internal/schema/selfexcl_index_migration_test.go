package schema

import (
	"strings"
	"testing"
)

// perf v1.8.3 (claude/perf-selfexcl): /value reads credited actions
// through their own index, and no sre_investigations index keys
// updated_at, which every lease, step and budget update rewrites (perf
// gate: 43 % HOT updates on sage.sre_investigations).

func TestSelfExclIndexMigration_CreatesTheIndexes(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	want := map[string][]string{
		"idx_action_log_value_credit": {"(executed_at) INCLUDE (action_type, " +
			"toil_minutes_saved) WHERE ((outcome = 'success'::text) AND " +
			"(toil_minutes_saved IS NOT NULL))"},
		"idx_sre_investigations_queue": {"(deployment_id, database_id, state, created_at)"},
		"idx_action_log_drop_index": {"(executed_at) WHERE (action_type = " +
			"'drop_index'::text)"},
		"idx_action_log_rolled_back": {"(measured_at) WHERE (outcome = " +
			"'rolled_back'::text)"},
	}
	for name, parts := range want {
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

// No index of sage.sre_investigations keys, computes or filters on
// updated_at, so an update that only moves it (and unindexed columns) is
// heap-only.
func TestSelfExclIndexMigration_NoInvestigationIndexUsesUpdatedAt(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	rows, err := pool.Query(ctx, `SELECT pg_get_indexdef(i.indexrelid)
		  FROM pg_index i WHERE i.indrelid = 'sage.sre_investigations'::regclass`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var def string
		if err := rows.Scan(&def); err != nil {
			t.Fatal(err)
		}
		n++
		if strings.Contains(def, "updated_at") {
			t.Errorf("index uses updated_at: %s", def)
		}
	}
	if n < 5 {
		t.Fatalf("sage.sre_investigations has %d indexes, want its full set", n)
	}
}

// An existing database still carrying the retired indexes loses them at
// the next bootstrap; an existing new index is left alone (same oid).
func TestSelfExclIndexMigration_RetiresOldInvestigationIndexes(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	for _, ddl := range []string{
		"CREATE INDEX IF NOT EXISTS sre_investigation_queue ON sage.sre_investigations " +
			"(deployment_id, database_id, state, updated_at)",
		"CREATE INDEX IF NOT EXISTS sre_investigation_retention ON " +
			"sage.sre_investigations (deployment_id, database_id, updated_at) WHERE NOT pinned",
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	var before uint32
	if err := pool.QueryRow(ctx, `SELECT 'sage.idx_action_log_value_credit'::regclass::oid`).
		Scan(&before); err != nil {
		t.Fatal(err)
	}
	bootstrapWithRetry(t, ctx, pool)
	var queue, retention bool
	var after uint32
	if err := pool.QueryRow(ctx, `SELECT to_regclass('sage.sre_investigation_queue') IS NOT NULL,
		to_regclass('sage.sre_investigation_retention') IS NOT NULL,
		'sage.idx_action_log_value_credit'::regclass::oid`).
		Scan(&queue, &retention, &after); err != nil {
		t.Fatal(err)
	}
	if queue || retention {
		t.Fatalf("retired indexes still present: queue=%v retention=%v", queue, retention)
	}
	if after != before {
		t.Fatal("an existing valid index was rebuilt")
	}
}
