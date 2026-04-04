package optimizer

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

func TestClassifyWorkload(t *testing.T) {
	tests := []struct {
		name       string
		writeRate  float64
		liveTuples int64
		want       string
	}{
		{"high write rate", 80, 1000, "oltp_write"},
		{"low write high tuples", 5, 500000, "olap"},
		{"low write low tuples", 5, 50000, "oltp_read"},
		{"moderate write under 30", 25, 1000, "oltp_read"},
		{"mixed workload", 50, 1000, "htap"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyWorkload(tt.writeRate, tt.liveTuples)
			if got != tt.want {
				t.Errorf("classifyWorkload(%v, %v) = %q, want %q",
					tt.writeRate, tt.liveTuples, got, tt.want)
			}
		})
	}
}

func TestComputeWriteRate(t *testing.T) {
	tests := []struct {
		name string
		ts   collector.TableStats
		want float64
	}{
		{
			name: "all zeros",
			ts:   collector.TableStats{},
			want: 0,
		},
		{
			name: "100 percent writes",
			ts:   collector.TableStats{NTupIns: 100},
			want: 100,
		},
		{
			name: "50 50 split",
			ts:   collector.TableStats{SeqScan: 50, NTupIns: 50},
			want: 50,
		},
		{
			name: "reads only",
			ts:   collector.TableStats{SeqScan: 100},
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeWriteRate(tt.ts)
			if got != tt.want {
				t.Errorf("computeWriteRate() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExtractTablesFromQuery_Simple(t *testing.T) {
	tables := extractTablesFromQuery("SELECT * FROM orders")
	if len(tables) != 1 || tables[0] != "public.orders" {
		t.Errorf("expected [public.orders], got %v", tables)
	}
}

func TestExtractTablesFromQuery_SchemaQualified(t *testing.T) {
	tables := extractTablesFromQuery("SELECT * FROM myschema.orders")
	if len(tables) != 1 || tables[0] != "myschema.orders" {
		t.Errorf("expected [myschema.orders], got %v", tables)
	}
}

func TestExtractTablesFromQuery_Join(t *testing.T) {
	tables := extractTablesFromQuery("SELECT * FROM orders JOIN items ON orders.id = items.order_id")
	if len(tables) != 2 {
		t.Fatalf("expected 2 tables, got %d: %v", len(tables), tables)
	}
	if tables[0] != "public.orders" {
		t.Errorf("expected public.orders first, got %s", tables[0])
	}
	if tables[1] != "public.items" {
		t.Errorf("expected public.items second, got %s", tables[1])
	}
}

func TestExtractTablesFromQuery_Update(t *testing.T) {
	tables := extractTablesFromQuery("UPDATE orders SET x = 1")
	if len(tables) != 1 || tables[0] != "public.orders" {
		t.Errorf("expected [public.orders], got %v", tables)
	}
}

func TestExtractTablesFromQuery_Insert(t *testing.T) {
	tables := extractTablesFromQuery("INSERT INTO orders VALUES (1)")
	if len(tables) != 1 || tables[0] != "public.orders" {
		t.Errorf("expected [public.orders], got %v", tables)
	}
}

func TestExtractTablesFromQuery_PgTable(t *testing.T) {
	tables := extractTablesFromQuery("SELECT * FROM pg_stat_activity")
	if len(tables) != 0 {
		t.Errorf("expected empty slice for pg_ table, got %v", tables)
	}
}

func TestExtractTablesFromQuery_Subquery(t *testing.T) {
	tables := extractTablesFromQuery(
		"SELECT * FROM orders WHERE id IN (SELECT id FROM items)",
	)
	if len(tables) != 2 {
		t.Fatalf("expected 2 tables, got %d: %v", len(tables), tables)
	}
	if tables[0] != "public.orders" {
		t.Errorf("expected public.orders first, got %s", tables[0])
	}
	if tables[1] != "public.items" {
		t.Errorf("expected public.items second, got %s", tables[1])
	}
}

func TestSkipSchema(t *testing.T) {
	tests := []struct {
		schema string
		want   bool
	}{
		{"sage", true},
		{"pg_catalog", true},
		{"public", false},
	}
	for _, tt := range tests {
		t.Run(tt.schema, func(t *testing.T) {
			got := skipSchema(tt.schema)
			if got != tt.want {
				t.Errorf("skipSchema(%q) = %v, want %v", tt.schema, got, tt.want)
			}
		})
	}
}

func TestFilterByMinCalls(t *testing.T) {
	queries := []QueryInfo{
		{QueryID: 1, Calls: 5},
		{QueryID: 2, Calls: 10},
		{QueryID: 3, Calls: 15},
	}
	got := filterByMinCalls(queries, 10)
	if len(got) != 2 {
		t.Fatalf("expected 2 queries, got %d", len(got))
	}
	if got[0].Calls != 10 || got[1].Calls != 15 {
		t.Errorf("unexpected queries: %v", got)
	}
}

func TestFilterByMinCalls_Empty(t *testing.T) {
	got := filterByMinCalls(nil, 10)
	if len(got) != 0 {
		t.Errorf("expected empty result, got %v", got)
	}
}

func TestCountIndexes(t *testing.T) {
	indexes := []collector.IndexStats{
		{SchemaName: "public", RelName: "orders", IndexRelName: "idx1"},
		{SchemaName: "public", RelName: "orders", IndexRelName: "idx2"},
		{SchemaName: "public", RelName: "items", IndexRelName: "idx3"},
	}
	got := countIndexes(indexes, "public", "orders")
	if got != 2 {
		t.Errorf("expected 2, got %d", got)
	}
}

func TestBuildPartitionParentSet(t *testing.T) {
	snap := &collector.Snapshot{
		Partitions: []collector.PartitionInfo{
			{
				ParentSchema: "public", ParentTable: "events",
				ChildSchema: "public", ChildTable: "events_2024",
			},
			{
				ParentSchema: "public", ParentTable: "events",
				ChildSchema: "public", ChildTable: "events_2025",
			},
		},
	}
	parents := buildPartitionParentSet(snap)
	if !parents["public.events"] {
		t.Error("expected public.events to be a partition parent")
	}
	if parents["public.events_2024"] {
		t.Error("child should not be in parent set")
	}
}

func TestPartitionedParentDetection(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "events", NLiveTup: 1000},
			{SchemaName: "public", RelName: "events_2024", NLiveTup: 400},
			{SchemaName: "public", RelName: "events_2025", NLiveTup: 300},
			{SchemaName: "public", RelName: "events_2026", NLiveTup: 300},
		},
		Queries: []collector.QueryStats{
			{
				QueryID:       1,
				Query:         "SELECT * FROM events WHERE id = $1",
				Calls:         100,
				MeanExecTime:  5.0,
				TotalExecTime: 500.0,
			},
			{
				QueryID:       2,
				Query:         "SELECT * FROM events_2024 WHERE ts > $1",
				Calls:         50,
				MeanExecTime:  3.0,
				TotalExecTime: 150.0,
			},
		},
		Partitions: []collector.PartitionInfo{
			{
				ParentSchema: "public", ParentTable: "events",
				ChildSchema: "public", ChildTable: "events_2024",
			},
			{
				ParentSchema: "public", ParentTable: "events",
				ChildSchema: "public", ChildTable: "events_2025",
			},
			{
				ParentSchema: "public", ParentTable: "events",
				ChildSchema: "public", ChildTable: "events_2026",
			},
		},
	}

	childSet := buildPartitionChildSet(snap)
	parentSet := buildPartitionParentSet(snap)
	tableQueries := groupQueriesByTable(snap)

	// Verify child set has all 3 children.
	for _, name := range []string{
		"public.events_2024", "public.events_2025", "public.events_2026",
	} {
		if !childSet[name] {
			t.Errorf("expected %s in child set", name)
		}
	}

	// Verify parent detected.
	if !parentSet["public.events"] {
		t.Fatal("expected public.events in parent set")
	}

	// Verify mergeChildQueries folds child queries into parent.
	parentQueries := tableQueries["public.events"]
	merged := mergeChildQueries(
		parentQueries, tableQueries, childSet, snap, "public.events",
	)
	seen := make(map[int64]bool)
	for _, q := range merged {
		seen[q.QueryID] = true
	}
	if !seen[1] {
		t.Error("expected query 1 (parent query) in merged set")
	}
	if !seen[2] {
		t.Error("expected query 2 (child query) folded into parent")
	}

	// Simulate the main loop: only parent should produce a context.
	var contextCount int
	var partitionedCount int
	for _, ts := range snap.Tables {
		key := ts.SchemaName + "." + ts.RelName
		if childSet[key] {
			continue
		}
		contextCount++
		if parentSet[key] {
			partitionedCount++
		}
	}
	if contextCount != 1 {
		t.Errorf("expected 1 context (parent only), got %d", contextCount)
	}
	if partitionedCount != 1 {
		t.Errorf("expected 1 partitioned parent, got %d", partitionedCount)
	}
}

func TestExtractColumnsFromQueries(t *testing.T) {
	queries := []QueryInfo{
		{Text: "SELECT * FROM orders WHERE status = $1 AND region = $2"},
		{Text: "SELECT * FROM items WHERE price > $1 ORDER BY name"},
	}
	cols := extractColumnsFromQueries(queries)
	if len(cols) == 0 {
		t.Fatal("expected at least one column extracted")
	}
	found := make(map[string]bool)
	for _, c := range cols {
		found[c] = true
	}
	for _, want := range []string{"status", "region", "price", "name"} {
		if !found[want] {
			t.Errorf("expected column %q in results %v", want, cols)
		}
	}
}

func TestExtractColumnsFromQueries_Empty(t *testing.T) {
	cols := extractColumnsFromQueries(nil)
	if len(cols) != 0 {
		t.Errorf("expected empty, got %v", cols)
	}
}

func TestExtractColumnRefs_WhereAndOrderBy(t *testing.T) {
	cols := extractColumnRefs(
		"SELECT * FROM t WHERE id = 1 AND name = 'x' ORDER BY created_at",
	)
	found := make(map[string]bool)
	for _, c := range cols {
		found[c] = true
	}
	if !found["id"] {
		t.Error("expected 'id'")
	}
	if !found["name"] {
		t.Error("expected 'name'")
	}
	if !found["created_at"] {
		t.Error("expected 'created_at'")
	}
}

func TestCleanColumnRef(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"status=", "status"},
		{"t.name", "name"},
		{`"Region"`, "region"},
		{"$1", ""},
		{"SELECT", ""},
		{"42", ""},
		{"", ""},
		{"orders.customer_id=", "customer_id"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := cleanColumnRef(tt.input)
			if got != tt.want {
				t.Errorf("cleanColumnRef(%q) = %q, want %q",
					tt.input, got, tt.want)
			}
		})
	}
}

func TestTotalQueryTime(t *testing.T) {
	queries := []QueryInfo{
		{TotalTimeMs: 100.5},
		{TotalTimeMs: 200.0},
		{TotalTimeMs: 50.5},
	}
	got := totalQueryTime(queries)
	if got != 351.0 {
		t.Errorf("totalQueryTime = %v, want 351.0", got)
	}
}

func TestTotalQueryTime_Empty(t *testing.T) {
	got := totalQueryTime(nil)
	if got != 0 {
		t.Errorf("totalQueryTime(nil) = %v, want 0", got)
	}
}

func TestMergeChildQueries_NoDuplicates(t *testing.T) {
	snap := &collector.Snapshot{
		Partitions: []collector.PartitionInfo{
			{
				ParentSchema: "public", ParentTable: "orders",
				ChildSchema: "public", ChildTable: "orders_q1",
			},
		},
	}
	// Same query ID hits both parent and child.
	tableQueries := map[string][]QueryInfo{
		"public.orders":    {{QueryID: 10, Calls: 50}},
		"public.orders_q1": {{QueryID: 10, Calls: 25}},
	}
	childSet := map[string]bool{"public.orders_q1": true}

	merged := mergeChildQueries(
		tableQueries["public.orders"],
		tableQueries,
		childSet,
		snap,
		"public.orders",
	)
	if len(merged) != 1 {
		t.Errorf("expected 1 query (deduped), got %d", len(merged))
	}
}
