package facts

import (
	"testing"
)

// evid-03b review on real PostgreSQL: the catalog resolver lists a table's
// indexes, so a do_not_touch fact on one index holds a REINDEX TABLE of its
// table, and nothing else on the table that leaves the index alone.
// No concurrent access tests: one read-only catalog query per Bind.

func TestDoNotTouchIndexHoldsReindexTableOnLiveDB(t *testing.T) {
	pool, ctx := livePool(t)
	table, index := doNotTouchTable(t, ctx, pool)
	other := uniqueName("idx_dnt_other_")
	execAll(t, ctx, pool, "CREATE INDEX "+other+" ON public."+table+" (id)")
	s := NewStore(pool)
	f, err := s.Declare(ctx, Proposal{Type: TypeDoNotTouch, Kind: KindIndex,
		Subject: "public." + index}, "alice@example.com", "")
	if err != nil {
		t.Fatalf("declare: %v", err)
	}
	t.Cleanup(func() { _, _ = s.Expire(ctx, f.ID, "test", "cleanup") })
	b := NewBinder(s, NewCatalogResolver(pool), nil)
	got := liveBind(t, ctx, b, "reindex_table", "REINDEX TABLE CONCURRENTLY public."+table,
		true, false)
	if len(got) != 1 || got[0].FactID != f.ID || got[0].Route != "hold" ||
		got[0].Object != "public."+index {
		t.Fatalf("REINDEX TABLE: bindings %+v, want fact %d held on %s", got, f.ID, index)
	}
	for _, sql := range []string{"VACUUM public." + table,
		"REINDEX INDEX CONCURRENTLY public." + other} {
		if got := liveBind(t, ctx, b, "reindex_index", sql, true, false); got != nil {
			t.Fatalf("%s: bound %+v", sql, got)
		}
	}
}
