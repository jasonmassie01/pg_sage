package tuning

import (
	"context"
	"strings"
	"testing"
)

// Relations is the deterministic evidence that may resolve an open
// finding: which tables and indexes still exist, and the valid index
// definitions of the tables, read in one catalog pass.
func TestPostgresStore_Relations(t *testing.T) {
	pool := dbPool(t)
	s := freshSchema(t, pool)
	mustExec(t, pool, "CREATE TABLE "+s+".t (a int, b int)")
	mustExec(t, pool, "CREATE INDEX t_ab ON "+s+".t (a, b)")
	store := pgStore(t, pool)
	tables := []string{s + ".t", s + ".gone", "no_such_schema.x", "not a name("}
	indexes := []string{s + ".t_ab", s + ".old_idx"}
	st, err := store.Relations(context.Background(), tables, indexes)
	if err != nil {
		t.Fatalf("relations: %v", err)
	}
	if !st.Tables[s+".t"] || st.Tables[s+".gone"] || st.Tables["no_such_schema.x"] {
		t.Fatalf("tables = %v", st.Tables)
	}
	if v, known := st.Tables["not a name("]; v || known {
		t.Fatalf("an unparseable name is unknown, never reported gone: %v", st.Tables)
	}
	if !st.Indexes[s+".t_ab"] || st.Indexes[s+".old_idx"] {
		t.Fatalf("indexes = %v", st.Indexes)
	}
	defs := st.IndexDefs[s+".t"]
	if len(defs) != 1 || !strings.Contains(defs[0], "(a, b)") {
		t.Fatalf("index defs = %v", st.IndexDefs)
	}
	if empty, err := store.Relations(context.Background(), nil, nil); err != nil ||
		len(empty.Tables) != 0 {
		t.Fatalf("nothing to check: %+v %v", empty, err)
	}
}

// InFlightIndexes are the index creates waiting in the action queue.
func TestPostgresStore_InFlightIndexes(t *testing.T) {
	pool := dbPool(t)
	ddl := "CREATE INDEX CONCURRENTLY inflight_probe_idx ON public.inflight_probe (a)"
	mustExec(t, pool, "DELETE FROM sage.action_queue WHERE proposed_sql LIKE "+
		"'%inflight_probe%'")
	mustExec(t, pool, `INSERT INTO sage.action_queue (proposed_sql, action_risk, status)
		VALUES ($1, 'safe', 'pending'), ($2, 'safe', 'rejected'),
		       ('ALTER SYSTEM SET work_mem = ''8MB''', 'moderate', 'pending')`, ddl,
		"CREATE INDEX CONCURRENTLY inflight_probe_old ON public.inflight_probe (b)")
	t.Cleanup(func() {
		mustExec(t, pool, "DELETE FROM sage.action_queue WHERE proposed_sql LIKE "+
			"'%inflight_probe%'")
	})
	got, err := pgStore(t, pool).InFlightIndexes(context.Background())
	if err != nil {
		t.Fatalf("in-flight: %v", err)
	}
	seen := map[string]bool{}
	for _, d := range got {
		seen[d] = true
	}
	if !seen[ddl] || seen["CREATE INDEX CONCURRENTLY inflight_probe_old ON "+
		"public.inflight_probe (b)"] {
		t.Fatalf("in-flight = %v", got)
	}
}
