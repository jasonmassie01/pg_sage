package schema

import (
	"testing"
)

// sage.index_replace is the durable state machine of the executor's
// replace action (roadmap 2.3): one row per replacement, its step, the old
// index's identity and definition (the soft drop), and its verification
// phase. The migration is idempotent and the state is constrained.

func TestIndexReplaceMigration(t *testing.T) {
	pool, ctx := requireDB(t)
	var before uint32
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
		var oid uint32
		if err := pool.QueryRow(ctx, `SELECT COALESCE(to_regclass(
			'sage.index_replace')::oid, 0)`).Scan(&oid); err != nil {
			t.Fatal(err)
		}
		if oid == 0 {
			t.Fatal("sage.index_replace missing")
		}
		if run == 1 && oid != before {
			t.Fatalf("a re-run rebuilt the table (oid %d -> %d)", before, oid)
		}
		before = oid
	}
	for _, column := range []string{"id", "database_id", "finding_id", "action_log_id",
		"approved_by", "table_name", "new_index", "new_index_oid", "create_sql",
		"drop_sql", "old_index", "old_index_oid", "old_definition", "rollback_sql",
		"state", "verify_phase", "before_state", "error", "created_at", "updated_at"} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
			WHERE table_schema = 'sage' AND table_name = 'index_replace'
			  AND column_name = $1`, column).Scan(&n); err != nil || n != 1 {
			t.Fatalf("column %s: %d, %v", column, n, err)
		}
	}
	insert := `INSERT INTO sage.index_replace (table_name, new_index, create_sql,
		drop_sql, old_index, old_index_oid, old_definition, rollback_sql, state)
		VALUES ('public.t', 'public.n', 'c', 'd', 'public.o', 1, 'def', 'r', $1)
		RETURNING id`
	var id int64
	if err := pool.QueryRow(ctx, insert, "creating").Scan(&id); err != nil {
		t.Fatalf("insert a creating row: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM sage.index_replace WHERE id=$1", id) })
	if err := pool.QueryRow(ctx, insert, "half_done").Scan(new(int64)); err == nil {
		t.Fatal("an unknown state is refused")
	}
	var phase string
	if err := pool.QueryRow(ctx, `SELECT verify_phase FROM sage.index_replace
		WHERE id = $1`, id).Scan(&phase); err != nil || phase != "none" {
		t.Fatalf("a new row has no verification phase yet: %q, %v", phase, err)
	}
}
