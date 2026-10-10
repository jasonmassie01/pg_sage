package autonomy

import (
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// The incremental structural scan (v2.3.x): which pass a cycle runs is a
// pure function of the cache and the catalog change counters, and the
// answer is a pure function of the per-table column summaries. Both are
// tested here without a database; schema_structural_incremental_db_test.go
// proves the passes against real PostgreSQL.
//
// No concurrent access tests in this file: plan and structuralInvariants
// read only their arguments. The cache's mutex is exercised by
// TestStructuralIncremental_ConcurrentCallersShareOnePass.

func TestStructuralPlan_Boundaries(t *testing.T) {
	t0 := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	marks := catalogMarks{changes: 10, attributeUpdates: 4}
	scanned := func(fullAge time.Duration) *structuralCache {
		return &structuralCache{scanned: true, at: t0, fullAt: t0.Add(-fullAge),
			marks: marks}
	}
	moved := catalogMarks{changes: 11, attributeUpdates: 4}
	cases := []struct {
		name  string
		cache *structuralCache
		now   time.Time
		marks catalogMarks
		want  structuralPass
	}{
		{"never scanned", &structuralCache{}, t0, marks, structuralPassFull},
		{"unchanged a minute later", scanned(0), t0.Add(time.Minute), marks,
			structuralPassNone},
		{"changed inside the minimum interval", scanned(0),
			t0.Add(structuralMinInterval - time.Nanosecond), moved, structuralPassNone},
		{"changed at exactly the minimum interval", scanned(0),
			t0.Add(structuralMinInterval), moved, structuralPassChanged},
		{"column rows updated", scanned(0), t0.Add(structuralMinInterval),
			catalogMarks{changes: 11, attributeUpdates: 5}, structuralPassVerified},
		{"counters reset", scanned(0), t0.Add(structuralMinInterval),
			catalogMarks{}, structuralPassVerified},
		{"unchanged just under the hour", scanned(0),
			t0.Add(structuralMaxAge - time.Nanosecond), marks, structuralPassNone},
		{"unchanged at exactly the hour", scanned(0), t0.Add(structuralMaxAge), marks,
			structuralPassVerified},
		{"changed just under a day since the full scan",
			scanned(structuralFullInterval - structuralMinInterval - time.Nanosecond),
			t0.Add(structuralMinInterval), moved, structuralPassChanged},
		{"a day since the full scan", scanned(structuralFullInterval - time.Minute),
			t0.Add(time.Minute), marks, structuralPassFull},
		{"clock moved backwards", scanned(0), t0.Add(-time.Second), marks,
			structuralPassFull},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.cache.plan(c.now, c.marks); got != c.want {
				t.Fatalf("plan = %v, want %v", got, c.want)
			}
		})
	}
}

func invariantRow(item schemaguard.Invariant) string {
	return fmt.Sprintf("%s|%s|%s|%s", item.Schema, item.Table, item.Subject, item.Kind)
}

// The answer is ordered like the full scan's ORDER BY schema, table,
// kind, column (name columns compare bytewise, C collation), every
// tightening candidate carries its proposal, and a table needs three
// columns, all text, to be everything_text.
func TestStructuralInvariants_OrderAndThresholds(t *testing.T) {
	tables := map[uint32]structuralTable{
		1: {schema: "b", name: "t", columns: 3, textColumns: 3,
			tightening: []string{"z_id", "a_count"}},
		2: {schema: "a", name: "u", columns: 2, textColumns: 2},
		3: {schema: "a", name: "t", columns: 4, textColumns: 1,
			tightening: []string{"count_text"}},
		4: {schema: "a", name: "empty"},
		5: {schema: "Z", name: "t", columns: 3, textColumns: 2,
			tightening: []string{"order_number"}},
		6: {schema: "a", name: "t2", columns: 3, textColumns: 3},
	}
	items := structuralInvariants(tables)
	want := []string{
		"Z|t|order_number|type_tightening",
		"a|t|count_text|type_tightening",
		"a|t2||everything_text",
		"b|t||everything_text",
		"b|t|a_count|type_tightening",
		"b|t|z_id|type_tightening",
	}
	if len(items) != len(want) {
		t.Fatalf("%d items, want %d: %+v", len(items), len(want), items)
	}
	for i, item := range items {
		if got := invariantRow(item); got != want[i] {
			t.Errorf("item %d = %s, want %s", i, got, want[i])
		}
		wantSQL := ""
		if item.Kind == schemaguard.InvariantTypeTightening {
			wantSQL = typeTighteningProposal(item.Schema, item.Table, item.Subject)
		}
		if item.ProposedSQL != wantSQL {
			t.Errorf("item %d proposal = %q, want %q", i, item.ProposedSQL, wantSQL)
		}
	}
}

func TestStructuralInvariants_NoTables(t *testing.T) {
	for _, tables := range []map[uint32]structuralTable{nil, {}} {
		if items := structuralInvariants(tables); len(items) != 0 {
			t.Fatalf("no tables gave %d items: %+v", len(items), items)
		}
	}
}
