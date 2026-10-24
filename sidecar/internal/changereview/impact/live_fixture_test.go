package impact

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/rvfixture"
)

// CHECK-IMP-01: the shared fixture builds schema rv identically on every
// version: the same columns and types, row counts, the created_at index,
// the workload counted over the extended protocol, two query_store
// samples 5 minutes apart and fresh statistics.
func TestFixtureBuildsSchemaRV(t *testing.T) {
	e := newEnv(t)
	f := e.f
	var cols string
	if err := f.Pool.QueryRow(e.ctx, `SELECT string_agg(c.relname || '.' || a.attname ||
		':' || format_type(a.atttypid, a.atttypmod), ',' ORDER BY c.relname, a.attnum)
		FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'rv' AND c.relkind = 'r' AND a.attnum > 0
		  AND NOT a.attisdropped`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	want := "rv_items.id:bigint,rv_items.order_id:integer,rv_items.qty:integer," +
		"rv_orders.id:bigint,rv_orders.email:text,rv_orders.status:text," +
		"rv_orders.code:character varying(10)," +
		"rv_orders.created_at:timestamp with time zone,rv_orders.customer_id:integer"
	if cols != want {
		t.Fatalf("rv columns:\n got %s\nwant %s", cols, want)
	}
	if n := f.Int64("SELECT count(*) FROM rv.rv_orders"); n != rvfixture.OrdersRows {
		t.Fatalf("rv_orders has %d rows, want %d", n, rvfixture.OrdersRows)
	}
	if n := f.Int64("SELECT count(*) FROM rv.rv_items"); n != rvfixture.ItemsRows {
		t.Fatalf("rv_items has %d rows, want %d", n, rvfixture.ItemsRows)
	}
	if n := f.Int64(`SELECT count(*) FROM pg_indexes WHERE schemaname = 'rv'
		AND indexname = 'rv_orders_created_idx'`); n != 1 {
		t.Fatalf("rv_orders_created_idx: %d indexes", n)
	}
	checkWorkload(t, f)
	gap := f.SampledTo.Sub(f.SampledFrom)
	if gap < rvfixture.SampleGap || gap > rvfixture.SampleGap+5e9 {
		t.Fatalf("samples are %v apart, want %v", gap, rvfixture.SampleGap)
	}
	if n := f.Int64(`SELECT count(*) FROM sage.query_store WHERE queryid = ANY($1)`,
		[]int64{f.Q1.ID, f.Q2.ID, f.Q3.ID}); n != 6 {
		t.Fatalf("query_store holds %d samples of Q1..Q3, want 6", n)
	}
	n := f.Int64(`SELECT reltuples::int8 FROM pg_class
		WHERE oid = 'rv.rv_orders'::regclass`)
	if n < 190000 || n > 210000 {
		t.Fatalf("rv_orders reltuples %d: ANALYZE did not run", n)
	}
	if n := f.Int64(`SELECT count(*) FROM pg_stats WHERE schemaname = 'rv'
		AND tablename = 'rv_orders'`); n != 6 {
		t.Fatalf("pg_stats has %d rv_orders columns, want 6", n)
	}
}

func checkWorkload(t *testing.T, f *rvfixture.Fixture) {
	t.Helper()
	for _, q := range []struct {
		q    rvfixture.Query
		want int64
	}{{f.Q1, rvfixture.Q1Calls}, {f.Q2, rvfixture.Q2Calls}, {f.Q3, rvfixture.Q3Calls}} {
		if q.q.Calls != q.want || q.q.ID == 0 {
			t.Errorf("%q: queryid %d with %d calls, want %d", q.q.Text, q.q.ID, q.q.Calls,
				q.want)
		}
	}
	if !strings.Contains(f.Q1.Text, "route=") {
		t.Errorf("Q1 has no sqlcommenter route tag: %q", f.Q1.Text)
	}
	if f.Q1.ID == f.Q2.ID || f.Q2.ID == f.Q3.ID {
		t.Errorf("workload queryids collide: %d %d %d", f.Q1.ID, f.Q2.ID, f.Q3.ID)
	}
	if n := f.Int64(`SELECT coalesce(sum(idx_scan), 0)::int8 FROM pg_stat_user_indexes
		WHERE indexrelname = 'rv_orders_created_idx'`); n == 0 {
		t.Errorf("Q3 never scanned rv_orders_created_idx")
	}
}
