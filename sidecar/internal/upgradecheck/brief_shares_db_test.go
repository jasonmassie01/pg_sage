package upgradecheck

import (
	"strings"
	"testing"
)

// drill-02c on a real server: the brief's query-time share comes from
// this database's pg_stat_statements, and is unknown without it.

func TestBriefDB_QueryTimeShareOrdersTheBusiestTableFirst(t *testing.T) {
	d := newDB(t, "")
	requireBelow18(t, d)
	var preloaded bool
	if err := d.pool.QueryRow(d.ctx, `SELECT 'pg_stat_statements' = ANY(
		string_to_array(replace(current_setting('shared_preload_libraries'), ' ', ''), ','))`).
		Scan(&preloaded); err != nil {
		t.Fatal(err)
	}
	target := Target{Major: 18, Provider: "self-managed"}
	b, err := BuildBrief(d.ctx, d.pool, target, BriefOptions{})
	if err != nil {
		t.Fatalf("BuildBrief: %v", err)
	}
	if b.QueryTime.State != SectionUnknown || !strings.Contains(b.QueryTime.Reason, "size") {
		t.Fatalf("without the extension = %+v, want unknown naming the size order",
			b.QueryTime)
	}
	d.exec(t, "CREATE EXTENSION pg_stat_statements",
		"CREATE TABLE public.busy AS SELECT g AS a FROM generate_series(1, 20000) g",
		"CREATE TABLE public.quiet (a int)")
	for range 5 {
		d.exec(t, "SELECT count(*) FROM public.busy b1 JOIN public.busy b2 USING (a)")
	}
	d.exec(t, "SELECT count(*) FROM public.quiet")
	b, err = BuildBrief(d.ctx, d.pool, target, BriefOptions{})
	if err != nil {
		t.Fatalf("BuildBrief: %v", err)
	}
	q := b.QueryTime
	if !preloaded {
		if q.State != SectionUnknown {
			t.Fatalf("extension not loaded = %+v, want unknown", q)
		}
		return
	}
	if q.State != SectionKnown || len(q.Items) < 2 || q.Items[0].Table != "public.busy" ||
		len(q.Items[0].QueryIDs) == 0 || q.Items[0].Share <= q.Items[1].Share ||
		q.Items[0].Share > 1 {
		t.Fatalf("query-time share = %+v, want public.busy first with its statement", q)
	}
}
