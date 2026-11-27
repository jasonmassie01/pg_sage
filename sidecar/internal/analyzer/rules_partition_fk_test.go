package analyzer

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/collector"
)

// rules-03: missing_fk_index on a partitioned table. PostgreSQL rejects
// CREATE INDEX CONCURRENTLY on the parent, and partitioned indexes are not
// in pg_stat_user_indexes, so coverage is read from PartitionedIndexes and
// an uncovered key gets the optimizer's partition plan, never SQL.
//
// No concurrent access tests: the rule is a pure function over one
// snapshot.

// fkPartSnap is public.orders partitioned into orders_p1 and orders_p2,
// with orders_customer_id_fkey on the parent cloned onto both partitions
// (as PostgreSQL does) and no index on customer_id anywhere.
func fkPartSnap() *collector.Snapshot {
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		{SchemaName: "public", RelName: "customers", Relkind: "r"},
		{SchemaName: "public", RelName: "orders", Relkind: "p"},
		{SchemaName: "public", RelName: "orders_p1", Relkind: "r"},
		{SchemaName: "public", RelName: "orders_p2", Relkind: "r"},
	}}
	for _, part := range []string{"orders_p1", "orders_p2"} {
		snap.Partitions = append(snap.Partitions, collector.PartitionInfo{
			ChildSchema: "public", ChildTable: part,
			ParentSchema: "public", ParentTable: "orders"})
	}
	for _, table := range []string{"orders", "orders_p1", "orders_p2"} {
		snap.ForeignKeys = append(snap.ForeignKeys, collector.ForeignKey{Schema: "public",
			TableName: table, ReferencedTable: "customers", Columns: []string{"customer_id"},
			ConstraintName: "orders_customer_id_fkey"})
	}
	return snap
}

func TestPartitionFK_ParentGetsPlanNotCIC(t *testing.T) {
	got := ruleMissingFKIndexes(fkPartSnap(), nil, nil, nil)
	if len(got) != 1 || got[0].ObjectIdentifier != "public.orders(customer_id)" {
		t.Fatalf("findings = %v, want the parent only (its plan covers the "+
			"partitions)", identsOf(got))
	}
	f := got[0]
	if f.RecommendedSQL != "" || f.RollbackSQL != "" {
		t.Fatalf("SQL = %q / %q, want none: CIC on a partitioned table is rejected",
			f.RecommendedSQL, f.RollbackSQL)
	}
	if f.Detail["partitioned_parent"] != true {
		t.Errorf("partitioned_parent = %v, want true", f.Detail["partitioned_parent"])
	}
	plan, _ := f.Detail["partition_plan"].([]string)
	if len(plan) != 5 {
		t.Fatalf("partition_plan = %q, want 1 + 2 per partition statements", plan)
	}
	wantFirst := `CREATE INDEX IF NOT EXISTS "orders_customer_id_fk_idx" ON ONLY ` +
		`"public"."orders" USING btree ("customer_id")`
	if plan[0] != wantFirst {
		t.Errorf("plan[0] = %q, want %q", plan[0], wantFirst)
	}
	if !strings.HasPrefix(plan[1], `CREATE INDEX CONCURRENTLY IF NOT EXISTS `+
		`"orders_customer_id_fk_idx_`) || !strings.Contains(plan[1], `ON "public"."orders_p1"`) {
		t.Errorf("plan[1] = %q, want the first partition's CIC", plan[1])
	}
	if !strings.HasPrefix(plan[2], `ALTER INDEX "public"."orders_customer_id_fk_idx" `+
		`ATTACH PARTITION "public"."orders_customer_id_fk_idx_`) {
		t.Errorf("plan[2] = %q, want the attach", plan[2])
	}
	for _, step := range plan {
		if strings.Contains(step, "CONCURRENTLY") && strings.Contains(step, `"public"."orders" `) {
			t.Errorf("plan step %q builds CONCURRENTLY on the parent", step)
		}
	}
	if !strings.Contains(f.Recommendation, "partition_plan") {
		t.Errorf("recommendation %q does not point at partition_plan", f.Recommendation)
	}
}

func TestPartitionFK_CoveredByPartitionedIndex(t *testing.T) {
	snap := fkPartSnap()
	snap.PartitionedIndexes = []collector.PartitionedIndex{
		partIndex("orders", "orders_cust_idx", 500, "customer_id", "id")}
	snap.Indexes = []collector.IndexStats{
		childIndex("orders_p1", "orders_p1_cust", "customer_id, id", 501, 500),
		childIndex("orders_p2", "orders_p2_cust", "customer_id, id", 502, 500),
	}
	if got := ruleMissingFKIndexes(snap, nil, nil, nil); len(got) != 0 {
		t.Fatalf("findings = %v, want none: the partitioned index covers the key",
			identsOf(got))
	}
}

func TestPartitionFK_UnusableParentIndexDoesNotCover(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*collector.PartitionedIndex)
	}{
		{"invalid", func(p *collector.PartitionedIndex) { p.IsValid = false }},
		{"partial", func(p *collector.PartitionedIndex) { p.IsPartial = true }},
		{"other leading column", func(p *collector.PartitionedIndex) {
			p.KeyColumns = []string{"id", "customer_id"}
		}},
		{"expression", func(p *collector.PartitionedIndex) { p.KeyColumns = []string{""} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := fkPartSnap()
			pi := partIndex("orders", "orders_cust_idx", 500, "customer_id")
			tc.mod(&pi)
			snap.PartitionedIndexes = []collector.PartitionedIndex{pi}
			got := ruleMissingFKIndexes(snap, nil, nil, nil)
			if len(got) != 1 || got[0].RecommendedSQL != "" {
				t.Fatalf("findings = %v, want the advisory parent finding", identsOf(got))
			}
		})
	}
}

func TestPartitionFK_SubPartitionedParentAdvisoryOnly(t *testing.T) {
	snap := fkPartSnap()
	snap.Tables[2].Relkind = "p" // orders_p1 is itself partitioned
	snap.Partitions = append(snap.Partitions, collector.PartitionInfo{
		ChildSchema: "public", ChildTable: "orders_p1a",
		ParentSchema: "public", ParentTable: "orders_p1"})
	snap.ForeignKeys = append(snap.ForeignKeys, collector.ForeignKey{Schema: "public",
		TableName: "orders_p1a", ReferencedTable: "customers", Columns: []string{"customer_id"},
		ConstraintName: "orders_customer_id_fkey"})
	got := ruleMissingFKIndexes(snap, nil, nil, nil)
	if len(got) != 1 || got[0].ObjectIdentifier != "public.orders(customer_id)" {
		t.Fatalf("findings = %v, want the top parent only", identsOf(got))
	}
	if _, ok := got[0].Detail["partition_plan"]; ok || got[0].RecommendedSQL != "" {
		t.Fatalf("sub-partitioned parent got a plan %v / SQL %q, want the note only",
			got[0].Detail["partition_plan"], got[0].RecommendedSQL)
	}
	if !strings.Contains(got[0].Recommendation, "sub-partitioned") {
		t.Errorf("recommendation %q lacks the sub-partition note", got[0].Recommendation)
	}
}

func TestPartitionFK_ParentWithoutPartitions(t *testing.T) {
	snap := fkPartSnap()
	snap.Partitions = nil
	snap.ForeignKeys = snap.ForeignKeys[:1]
	got := ruleMissingFKIndexes(snap, nil, nil, nil)
	if len(got) != 1 || got[0].RecommendedSQL != "" {
		t.Fatalf("findings = %v, want one advisory finding", identsOf(got))
	}
	if _, ok := got[0].Detail["partition_plan"]; ok {
		t.Errorf("plan %v for a parent with no partitions", got[0].Detail["partition_plan"])
	}
}

func TestPartitionFK_PartitionOwnKeyStillFlagged(t *testing.T) {
	snap := fkPartSnap()
	snap.ForeignKeys = []collector.ForeignKey{{Schema: "public", TableName: "orders_p1",
		ReferencedTable: "regions", Columns: []string{"region_id"}, ConstraintName: "p1_region_fk"}}
	got := ruleMissingFKIndexes(snap, nil, nil, nil)
	if len(got) != 1 || got[0].ObjectIdentifier != "public.orders_p1(region_id)" {
		t.Fatalf("findings = %v, want the partition's own key", identsOf(got))
	}
	want := `CREATE INDEX CONCURRENTLY ON "public"."orders_p1" ("region_id");`
	if got[0].RecommendedSQL != want {
		t.Errorf("SQL = %q, want %q (a leaf takes CIC)", got[0].RecommendedSQL, want)
	}
}

func TestPartitionFK_PlainTableUnchanged(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{{SchemaName: "public", RelName: "orders", Relkind: "r"}},
		ForeignKeys: []collector.ForeignKey{{Schema: "public", TableName: "orders",
			ReferencedTable: "customers", Columns: []string{"customer_id"}, ConstraintName: "fk"}},
	}
	got := ruleMissingFKIndexes(snap, nil, nil, nil)
	if len(got) != 1 {
		t.Fatalf("findings = %v, want one", identsOf(got))
	}
	want := `CREATE INDEX CONCURRENTLY ON "public"."orders" ("customer_id");`
	if got[0].RecommendedSQL != want {
		t.Errorf("SQL = %q, want %q", got[0].RecommendedSQL, want)
	}
	if _, ok := got[0].Detail["partitioned_parent"]; ok {
		t.Error("a plain table is not a partitioned parent")
	}
}

func TestFKIndexName_FitsIdentifierLimit(t *testing.T) {
	if got := fkIndexName("orders", []string{"customer_id"}); got !=
		"orders_customer_id_fk_idx" {
		t.Errorf("name = %q", got)
	}
	long := fkIndexName(strings.Repeat("t", 60), []string{strings.Repeat("c", 40)})
	if len(long) > 63 {
		t.Fatalf("name %q is %d bytes, over NAMEDATALEN-1", long, len(long))
	}
	other := fkIndexName(strings.Repeat("t", 60), []string{strings.Repeat("d", 40)})
	if long == other {
		t.Errorf("truncated names collide: %q", long)
	}
	multi := fkIndexName(strings.Repeat("é", 40), []string{"x"})
	if len(multi) > 63 || !utf8.ValidString(multi) || !strings.HasPrefix(multi, "é") {
		t.Errorf("multi-byte name %q (%d bytes) split a character or overran", multi,
			len(multi))
	}
}
