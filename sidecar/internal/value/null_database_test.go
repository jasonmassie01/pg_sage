package value

import (
	"testing"
)

// Regression test for SURF-03 / G2-B11: an action credited without a
// database attribution must not make the whole value report fail.
func TestReadSnapshotToleratesUnattributedCredit(t *testing.T) {
	pool, ctx := requireValuePostgres(t)
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome, toil_minutes_saved, toil_model_version)
		VALUES ('analyze_table', 'SELECT 1', 'success', 15, 1) RETURNING id`).
		Scan(&id); err != nil {
		t.Fatalf("insert unattributed credit: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.action_log WHERE id=$1", id)
	})

	snapshot, err := NewPostgresRepository(pool).ReadSnapshot(ctx, Filter{})

	if err != nil {
		t.Fatalf("ReadSnapshot with NULL database_id: %v", err)
	}
	if snapshot.AllTimeMinutes < 15 {
		t.Fatalf("unattributed credit missing from report: %#v", snapshot)
	}
}
