package analyzer

import (
	"reflect"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// collection-04: the collector reports one row for a key on a partitioned
// table (its clones are left out), and pg_stat_user_indexes never lists
// the partitioned parent's own index. missing_fk_index therefore checks
// the key on each leaf partition, and the unused-index rule protects the
// leaves' supporting indexes.
//
// No concurrency test: both rules are pure functions of one snapshot.

// partitionedEvents is app.events partitioned into events_p0 and
// events_p1 (events_p1 sub-partitioned into events_p1a), with a key on
// customer_id; only events_p0 has an index on it.
func partitionedEvents() *collector.Snapshot {
	tab := func(name, kind string) collector.TableStats {
		return collector.TableStats{SchemaName: "app", RelName: name, Relkind: kind}
	}
	part := func(parent, child string) collector.PartitionInfo {
		return collector.PartitionInfo{ParentSchema: "app", ParentTable: parent,
			ChildSchema: "app", ChildTable: child}
	}
	idx := func(table, name string) collector.IndexStats {
		return collector.IndexStats{SchemaName: "app", RelName: table, IndexRelName: name,
			IsValid: true, IndexDef: "CREATE INDEX " + name + " ON app." + table +
				" USING btree (customer_id)"}
	}
	return &collector.Snapshot{
		Tables: []collector.TableStats{tab("events", "p"), tab("events_p0", "r"),
			tab("events_p1", "p"), tab("events_p1a", "r")},
		Partitions: []collector.PartitionInfo{part("events", "events_p0"),
			part("events", "events_p1"), part("events_p1", "events_p1a")},
		Indexes: []collector.IndexStats{idx("events_p0", "events_p0_cust")},
		ForeignKeys: []collector.ForeignKey{{Schema: "app", TableName: "events",
			RefSchema: "app", ReferencedTable: "customers",
			ConstraintName: "events_customer_fk", Columns: []string{"customer_id"}}},
	}
}

func TestMissingFKIndex_PartitionedKeyCheckedOnLeaves(t *testing.T) {
	got := ruleMissingFKIndexes(partitionedEvents(), nil, nil, nil)
	if len(got) != 1 {
		t.Fatalf("findings = %+v, want one, for the unindexed leaf events_p1a", got)
	}
	want := `CREATE INDEX CONCURRENTLY ON "app"."events_p1a" ("customer_id");`
	if got[0].RecommendedSQL != want || got[0].Detail["partition_of"] != "app.events" {
		t.Fatalf("finding = %s %v, want %s on a partition of app.events",
			got[0].RecommendedSQL, got[0].Detail["partition_of"], want)
	}
}

// Boundary: every leaf indexed gives no finding (the parent's own index
// is never in the snapshot); unknown partitions give none either.
func TestMissingFKIndex_PartitionedAllLeavesIndexedOrUnknown(t *testing.T) {
	snap := partitionedEvents()
	snap.Indexes = append(snap.Indexes, collector.IndexStats{SchemaName: "app",
		RelName: "events_p1a", IndexRelName: "events_p1a_cust", IsValid: true,
		IndexDef: "CREATE INDEX events_p1a_cust ON app.events_p1a USING btree " +
			"(customer_id)"})
	if got := ruleMissingFKIndexes(snap, nil, nil, nil); len(got) != 0 {
		t.Fatalf("all leaves indexed: findings = %+v, want none", got)
	}
	snap = partitionedEvents()
	snap.Partitions = nil
	snap.Unavailable = map[string]string{"partitions": "statement timeout"}
	if got := ruleMissingFKIndexes(snap, nil, nil, nil); len(got) != 0 {
		t.Fatalf("partitions unknown: findings = %+v, want none (no guessing)", got)
	}
}

func TestBuildFKRequirements_PartitionedKeyProtectsLeaves(t *testing.T) {
	got := buildFKRequirements(partitionedEvents())
	cols := [][]string{{"customer_id"}}
	want := map[tableKey][][]string{{"app", "events"}: cols, {"app", "events_p0"}: cols,
		{"app", "events_p1a"}: cols}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requirements = %v, want %v", got, want)
	}
	extras := &RuleExtras{FirstSeen: map[string]time.Time{
		"app.events_p0_cust": time.Now().Add(-30 * 24 * time.Hour),
	}, RecentlyCreated: map[string]time.Time{}}
	if got := ruleUnusedIndexes(partitionedEvents(), nil, unusedCfg(), extras); len(got) != 0 {
		t.Fatalf("sole FK index of a leaf recommended for drop: %+v", got)
	}
	control := partitionedEvents()
	control.ForeignKeys = nil
	if got := ruleUnusedIndexes(control, nil, unusedCfg(), extras); len(got) != 1 {
		t.Fatalf("control without the key: findings = %+v, want the drop", got)
	}
}

// A partition cycle in a malformed snapshot terminates.
func TestFKTables_LeavesTerminatesOnCycle(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{{SchemaName: "s", RelName: "a", Relkind: "p"},
			{SchemaName: "s", RelName: "b", Relkind: "p"}},
		Partitions: []collector.PartitionInfo{
			{ParentSchema: "s", ParentTable: "a", ChildSchema: "s", ChildTable: "b"},
			{ParentSchema: "s", ParentTable: "b", ChildSchema: "s", ChildTable: "a"}},
	}
	if got := newFKTables(snap).leaves(tableKey{"s", "a"}); len(got) != 0 {
		t.Fatalf("leaves = %v, want none", got)
	}
}
