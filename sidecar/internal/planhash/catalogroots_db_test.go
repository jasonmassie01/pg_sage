package planhash

import (
	"testing"
)

// rootOf resolves one schema-qualified relation through the catalog read.
func rootOf(t *testing.T, r RootResolver, schema, rel string) Root {
	t.Helper()
	got, err := r.Root(schema, rel)
	if err != nil {
		t.Fatalf("Root(%q, %q): %v", schema, rel, err)
	}
	return got
}

// scanPlan names one relation the way EXPLAIN does; an empty schema is a
// plan captured without VERBOSE.
func scanPlan(schema, rel string) []byte {
	if schema == "" {
		return []byte(`{"Node Type": "Seq Scan", "Relation Name": "` + rel + `"}`)
	}
	return []byte(`{"Node Type": "Seq Scan", "Schema": "` + schema +
		`", "Relation Name": "` + rel + `"}`)
}

// The leaf count is what makes a full scan "all"; it must count leaf
// partitions only (not an intermediate sub-partitioned table) and follow
// partitions added after the first read.
func TestResolve_LeafCountsCountOnlyLeafPartitions(t *testing.T) {
	pool, ctx := livePool(t)
	dailyTable(t, ctx, pool, "phx_leaf", "2026-11-05", "2026-11-06")
	mustExec(t, ctx, pool, `CREATE TABLE phx_leaf.phx_events_nov
		PARTITION OF phx_leaf.phx_events FOR VALUES FROM ('2026-11-10') TO ('2026-12-01')
		PARTITION BY RANGE (ts)`)
	mustExec(t, ctx, pool, `CREATE TABLE phx_leaf.phx_events_nov_a
		PARTITION OF phx_leaf.phx_events_nov FOR VALUES FROM ('2026-11-10') TO ('2026-11-20')`)
	mustExec(t, ctx, pool, `CREATE TABLE phx_leaf.phx_events_nov_b
		PARTITION OF phx_leaf.phx_events_nov FOR VALUES FROM ('2026-11-20') TO ('2026-12-01')`)
	mustExec(t, ctx, pool, "CREATE TABLE phx_leaf.phx_plain (id int)")
	roots, err := Resolve(ctx, pool, scanPlan("phx_leaf", "phx_events_nov_a"),
		scanPlan("phx_leaf", "phx_events_p20261105"), scanPlan("phx_leaf", "phx_plain"),
		scanPlan("phx_leaf", "phx_events_p20261106_ts_idx"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := Root{Schema: "phx_leaf", Name: "phx_events", Leaves: 4}
	for _, rel := range []string{"phx_events_nov_a", "phx_events_p20261105"} {
		if got := rootOf(t, roots, "phx_leaf", rel); got != want {
			t.Errorf("root of %s = %+v, want %+v", rel, got, want)
		}
	}
	if got := rootOf(t, roots, "phx_leaf", "phx_plain"); got !=
		(Root{Schema: "phx_leaf", Name: "phx_plain"}) {
		t.Errorf("plain table root = %+v, want itself without leaves", got)
	}
	if got := rootOf(t, roots, "phx_leaf", "phx_events_p20261106_ts_idx"); got !=
		(Root{Schema: "phx_leaf", Name: "phx_events_ts_idx"}) {
		t.Errorf("partition index root = %+v, want the partitioned index", got)
	}
	addDay(t, ctx, pool, "phx_leaf", "2026-11-07")
	roots, err = Resolve(ctx, pool, scanPlan("phx_leaf", "phx_events_p20261105"))
	if err != nil {
		t.Fatalf("Resolve after a new partition: %v", err)
	}
	if got := rootOf(t, roots, "phx_leaf", "phx_events_p20261105"); got.Leaves != 5 {
		t.Errorf("leaf count after adding a partition = %d, want 5 (no stale count)",
			got.Leaves)
	}
}

// Without VERBOSE a name shared by plain tables in two schemas names its
// own root whichever table the plan scanned, so it resolves to itself; a
// shared name that is a partition anywhere stays unresolved.
func TestResolve_AmbiguousPlainNameResolvesToItself(t *testing.T) {
	pool, ctx := livePool(t)
	dailyTable(t, ctx, pool, "phx_twin_a", "2026-11-05")
	dailyTable(t, ctx, pool, "phx_twin_b", "2026-11-05")
	mustExec(t, ctx, pool, "CREATE TABLE phx_twin_a.phx_twin_plain (id int)")
	mustExec(t, ctx, pool, "CREATE TABLE phx_twin_b.phx_twin_plain (id int)")
	mustExec(t, ctx, pool, "CREATE INDEX phx_twin_plain_idx ON phx_twin_a.phx_twin_plain (id)")
	mustExec(t, ctx, pool, "CREATE INDEX phx_twin_plain_idx ON phx_twin_b.phx_twin_plain (id)")
	roots, err := Resolve(ctx, pool, scanPlan("", "phx_twin_plain"),
		scanPlan("", "phx_twin_plain_idx"), scanPlan("", "phx_events_p20261105"),
		scanPlan("", "phx_events"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, rel := range []string{"phx_twin_plain", "phx_twin_plain_idx"} {
		if got := rootOf(t, roots, "", rel); got != (Root{Name: rel}) {
			t.Errorf("shared plain name %s resolved to %+v, want itself", rel, got)
		}
	}
	for _, rel := range []string{"phx_events_p20261105", "phx_events"} {
		if got, err := roots.Root("", rel); err == nil {
			t.Errorf("shared partitioned name %s resolved to %+v, want unresolved", rel, got)
		}
	}
	h := resolvedHash(t, ctx, pool, scanPlan("", "phx_twin_plain"))
	if !resolvedPattern.MatchString(h) {
		t.Errorf("a plan on a shared plain table name hashed %s, want a v2 hash", h)
	}
}
