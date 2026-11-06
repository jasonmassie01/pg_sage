package facts

import "testing"

// executor-01: the soft-drop restore only re-creates an index pg_sage
// dropped itself, so like revert_created_index no fact binds it, even one
// that would bind the same CREATE INDEX as a fresh change.
func TestBindExemptsTheDropRestore(t *testing.T) {
	facts := []Fact{confirmed(12, TypeAppMigrations, KindIndex, "public.idx_thesis_*", nil)}
	sql := "CREATE INDEX CONCURRENTLY idx_thesis_x ON public.thesis USING btree (run_id)"
	if got := bindSQL(facts, "create_index_concurrently", sql, false); len(got) != 1 {
		t.Fatalf("a fresh create must stay bound: %+v", got)
	}
	for _, operator := range []bool{false, true} {
		if got := bindSQL(facts, "restore_dropped_index", sql, operator); len(got) != 0 {
			t.Fatalf("restore (operator=%v) bound: %+v", operator, got)
		}
	}
}
