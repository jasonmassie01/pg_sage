package tuning

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
)

// indexes-03 on real PostgreSQL: the agent proposes dropping a
// never-scanned index that supports a foreign key. Tuning refuses it (the
// unused_index rule owns unused drops and skips FK-only indexes), so no
// tuning_index_drop finding is stored. A covered_by drop in the same answer
// is admitted and stored, so the refusal is not an empty pipeline.
// No concurrent access test: one agent cycle runs under the agent's mutex.
func TestTune_NeverScannedFKIndexDropIsRefusedAndNotStored(t *testing.T) {
	pool := dbPool(t)
	s := freshSchema(t, pool)
	mustExec(t, pool, "CREATE TABLE "+s+".customers (id bigint PRIMARY KEY)")
	mustExec(t, pool, "CREATE TABLE "+s+".orders (id bigint PRIMARY KEY, "+
		"customer_id bigint REFERENCES "+s+".customers (id), status text, created_at "+
		"timestamptz)")
	mustExec(t, pool, "CREATE INDEX orders_customer_idx ON "+s+".orders (customer_id)")
	mustExec(t, pool, "CREATE INDEX orders_status_idx ON "+s+".orders (status)")
	mustExec(t, pool, "CREATE INDEX orders_status_created_idx ON "+s+
		".orders (status, created_at)")
	prev, cur := schemaOrdersPair(s)
	cur.Indexes = catalogIndexes(t, pool, s)
	cur.ForeignKeys = []collector.ForeignKey{{TableName: s + ".orders",
		ReferencedTable: s + ".customers", Columns: []string{"customer_id"},
		ConstraintName: "orders_customer_id_fkey"}}
	cur.System.RelationStatsEpoch = cur.CollectedAt.Add(-30 * 24 * time.Hour)
	h := newHarness(t, answer(`{"proposals":[`+
		`{"type":"index_drop","index":"`+s+`.orders_customer_idx","evidence":["T1"],`+
		`"rationale":"never scanned"},`+
		`{"type":"index_drop","index":"`+s+`.orders_status_idx","evidence":["T1"],`+
		`"rationale":"redundant"}]}`))
	out, err := h.agent.Tune(context.Background(), cur, prev)
	if err != nil {
		t.Fatalf("tune: %v", err)
	}
	if h.model.callCount() == 0 {
		t.Fatal("the model was never asked: the case did not form")
	}
	var drops []analyzer.Finding
	for _, f := range out.Findings {
		if f.Category == CategoryIndexDrop {
			drops = append(drops, f)
		}
	}
	if len(drops) != 1 || drops[0].ObjectIdentifier != s+".orders_status_idx" ||
		drops[0].Detail["covered_by"] != "orders_status_created_idx" {
		t.Fatalf("drops = %+v: only the covered index may be dropped", drops)
	}
	if err := analyzer.UpsertFindings(context.Background(), pool, out.Findings); err != nil {
		t.Fatalf("store findings: %v", err)
	}
	if n := storedDrops(t, pool, s+".orders_customer_idx"); n != 0 {
		t.Fatalf("%d tuning_index_drop findings stored for the FK index", n)
	}
	if n := storedDrops(t, pool, s+".orders_status_idx"); n != 1 {
		t.Fatalf("covered drop stored %d times, want 1", n)
	}
}

// schemaOrdersPair is ordersPair's interval on schema s.
func schemaOrdersPair(s string) (prev, cur *collector.Snapshot) {
	prev, cur = ordersPair()
	for _, snap := range []*collector.Snapshot{prev, cur} {
		for i := range snap.Queries {
			snap.Queries[i].Query = strings.ReplaceAll(snap.Queries[i].Query,
				"public.orders", s+".orders")
		}
		snap.Tables[0].SchemaName = s
		snap.Indexes = nil
	}
	return prev, cur
}

// catalogIndexes reads schema s's indexes as the collector reports them.
func catalogIndexes(t *testing.T, pool *pgxpool.Pool, s string) []collector.IndexStats {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT st.schemaname, st.relname, st.indexrelname, st.idx_scan, i.indisunique,
		       i.indisprimary, i.indisvalid, pg_get_indexdef(i.indexrelid),
		       pg_relation_size(i.indexrelid)
		  FROM pg_stat_user_indexes st JOIN pg_index i USING (indexrelid)
		 WHERE st.schemaname = $1`, s)
	if err != nil {
		t.Fatalf("read indexes: %v", err)
	}
	defer rows.Close()
	var out []collector.IndexStats
	for rows.Next() {
		ix := collector.IndexStats{IndexType: "btree"}
		if err := rows.Scan(&ix.SchemaName, &ix.RelName, &ix.IndexRelName, &ix.IdxScan,
			&ix.IsUnique, &ix.IsPrimary, &ix.IsValid, &ix.IndexDef,
			&ix.IndexBytes); err != nil {
			t.Fatalf("scan index: %v", err)
		}
		out = append(out, ix)
	}
	if err := rows.Err(); err != nil || len(out) != 5 {
		t.Fatalf("indexes = %+v (%v), want 5", out, err)
	}
	return out
}

func storedDrops(t *testing.T, pool *pgxpool.Pool, index string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.findings
		WHERE category = $1 AND object_identifier = $2`, CategoryIndexDrop,
		index).Scan(&n); err != nil {
		t.Fatalf("count findings: %v", err)
	}
	return n
}
