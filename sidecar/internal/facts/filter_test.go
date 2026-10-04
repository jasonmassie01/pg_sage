package facts

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
)

// Confirmed facts shape the analyzer's output: test-fixture schemas leave
// the snapshot (no performance findings, no budget spent on them) and come
// back as one cleanup batch per fact; DDL on app-owned objects becomes a
// source-fix packet instead of executable SQL.

func fixtureSnapshot() *collector.Snapshot {
	return &collector.Snapshot{
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "orders"},
			{SchemaName: "test_memory_aa11bb", RelName: "events"},
			{SchemaName: "test_memory_cc22dd", RelName: "events"},
			{SchemaName: "app_test", RelName: "x"},
		},
		Indexes: []collector.IndexStats{
			{SchemaName: "public", RelName: "orders", IndexRelName: "orders_pkey"},
			{SchemaName: "test_memory_aa11bb", RelName: "events", IndexRelName: "e_pkey"},
		},
	}
}

func TestFilterSnapshotRemovesConfirmedFixtureSchemas(t *testing.T) {
	proposed := confirmed(8, TypeTestFixture, KindSchema, "app_*", nil)
	proposed.Status = StatusProposed
	facts := []Fact{confirmed(7, TypeTestFixture, KindSchema, "test_memory_*", nil), proposed}
	snap := fixtureSnapshot()
	excluded := FilterSnapshot(facts, bindNow, snap)
	if !reflect.DeepEqual(excluded, []string{"test_memory_aa11bb", "test_memory_cc22dd"}) {
		t.Fatalf("excluded %v", excluded)
	}
	if len(snap.Tables) != 2 || snap.Tables[0].SchemaName != "public" ||
		snap.Tables[1].SchemaName != "app_test" || len(snap.Indexes) != 1 {
		t.Fatalf("snapshot after filter: %+v / %+v", snap.Tables, snap.Indexes)
	}
	if got := FilterSnapshot(nil, bindNow, fixtureSnapshot()); got != nil {
		t.Fatalf("no facts excluded %v", got)
	}
	if got := FilterSnapshot(facts, bindNow, nil); got != nil {
		t.Fatalf("nil snapshot excluded %v", got)
	}
}

func TestApplyFindingsRedirectsAppOwnedDDLToASourceFix(t *testing.T) {
	facts := []Fact{confirmed(12, TypeAppMigrations, KindTable, "public.thesis", nil)}
	in := []analyzer.Finding{
		{Category: "index_optimization", ObjectType: "index",
			ObjectIdentifier: "public.thesis|btree(run_id)", Title: "Index thesis",
			Recommendation: "Create an index.",
			RecommendedSQL: "CREATE INDEX CONCURRENTLY idx_t ON public.thesis (run_id)",
			RollbackSQL:    "DROP INDEX CONCURRENTLY public.idx_t", ActionRisk: "safe"},
		{Category: "unused_index", ObjectType: "index", ObjectIdentifier: "public.idx_x",
			RecommendedSQL: "DROP INDEX CONCURRENTLY public.idx_x", ActionRisk: "safe",
			Detail: map[string]any{"table": "public.orders"}},
	}
	out, evaluated := ApplyFindings(facts, in, nil, bindNow)
	if len(out) != 2 || !reflect.DeepEqual(evaluated, []string{CategoryTestFixtureCleanup}) {
		t.Fatalf("out %d, evaluated %v", len(out), evaluated)
	}
	got := out[0]
	if got.RecommendedSQL != "" || got.RollbackSQL != "" || got.ActionRisk != "" {
		t.Fatalf("redirected finding still executable: %+v", got)
	}
	fix, ok := got.Detail["source_fix"].(SourceFix)
	if !ok || fix.FactID != 12 || !strings.Contains(fix.Migration,
		"CREATE INDEX CONCURRENTLY idx_t ON public.thesis (run_id);") {
		t.Fatalf("source fix %#v", got.Detail["source_fix"])
	}
	if ids, _ := got.Detail["bound_by_facts"].([]int64); !reflect.DeepEqual(ids, []int64{12}) {
		t.Fatalf("bound_by_facts %#v", got.Detail["bound_by_facts"])
	}
	if !strings.Contains(got.Recommendation, "fact #12") {
		t.Fatalf("recommendation does not name the fact: %q", got.Recommendation)
	}
	if in[0].RecommendedSQL == "" {
		t.Fatal("the input finding was modified in place")
	}
	if out[1].RecommendedSQL != in[1].RecommendedSQL || out[1].Detail["source_fix"] != nil {
		t.Fatalf("an unrelated finding changed: %+v", out[1])
	}
}

func TestApplyFindingsDropsFixtureFindingsAndOffersOneCleanupBatch(t *testing.T) {
	facts := []Fact{confirmed(7, TypeTestFixture, KindSchema, "test_memory_*", nil)}
	in := []analyzer.Finding{
		{Category: "duplicate_index", ObjectIdentifier: "test_memory_aa11bb.idx_a",
			RecommendedSQL: "DROP INDEX CONCURRENTLY test_memory_aa11bb.idx_a"},
		{Category: "seq_scan_heavy", ObjectIdentifier: "public.orders"},
	}
	excluded := []string{"test_memory_aa11bb", "test_memory_cc22dd"}
	out, evaluated := ApplyFindings(facts, in, excluded, bindNow)
	if len(out) != 2 || out[0].ObjectIdentifier != "public.orders" ||
		!reflect.DeepEqual(evaluated, []string{CategoryTestFixtureCleanup}) {
		t.Fatalf("out %+v", out)
	}
	c := out[1]
	if c.Category != CategoryTestFixtureCleanup || c.ObjectIdentifier != "test_memory_*" ||
		c.RecommendedSQL != "" || c.Severity != "info" {
		t.Fatalf("cleanup finding %+v", c)
	}
	sql, _ := c.Detail["cleanup_sql"].(string)
	for _, want := range []string{`DROP SCHEMA IF EXISTS "test_memory_aa11bb" CASCADE;`,
		`DROP SCHEMA IF EXISTS "test_memory_cc22dd" CASCADE;`, "BEGIN;", "COMMIT;"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("cleanup batch lacks %q:\n%s", want, sql)
		}
	}
	if c.Detail["fact_id"] != int64(7) || c.Detail["schema_count"] != 2 ||
		!strings.Contains(c.Title, "fact #7") {
		t.Fatalf("cleanup detail %+v title %q", c.Detail, c.Title)
	}
	// No matching schema left: no cleanup finding, and the category is
	// still evaluated so the old one resolves.
	out, evaluated = ApplyFindings(facts, nil, nil, bindNow)
	if len(out) != 0 || len(evaluated) != 1 {
		t.Fatalf("after cleanup: %+v %v", out, evaluated)
	}
}

func TestApplyFindingsKeepsArchivesAndSlots(t *testing.T) {
	facts := []Fact{
		confirmed(5, TypeAppendOnly, KindTable, "app.audit_log", nil),
		confirmed(9, TypeSlotConsumer, KindSlot, "cdc_orders",
			map[string]string{"consumer": "debezium"}),
	}
	in := []analyzer.Finding{
		{Category: "table_bloat", ObjectIdentifier: "app.audit_log",
			RecommendedSQL: "VACUUM (FULL) app.audit_log"},
		{Category: "inactive_slot", ObjectIdentifier: "slot:cdc_orders",
			RecommendedSQL: "SELECT pg_drop_replication_slot('cdc_orders');"},
	}
	out, _ := ApplyFindings(facts, in, nil, bindNow)
	for i, f := range out {
		if f.RecommendedSQL != "" {
			t.Fatalf("finding %d still executable: %+v", i, f)
		}
		if _, ok := f.Detail["source_fix"].(SourceFix); !ok {
			t.Fatalf("finding %d carries no narrowing packet: %+v", i, f.Detail)
		}
	}
}

func TestFindingFilterPassesThroughWhenFactsAreUnavailable(t *testing.T) {
	var logged []string
	f := NewFindingFilter(&fakeConfirmed{err: errors.New("relation sage.facts missing")},
		func(_ string, format string, args ...any) { logged = append(logged, format) })
	snap := fixtureSnapshot()
	if got := f.ExcludeSnapshot(context.Background(), snap); got != nil ||
		len(snap.Tables) != 4 {
		t.Fatalf("excluded %v, tables %d", got, len(snap.Tables))
	}
	in := []analyzer.Finding{{Category: "unused_index", ObjectIdentifier: "public.i",
		RecommendedSQL: "DROP INDEX CONCURRENTLY public.i"}}
	out, evaluated := f.ApplyFindings(context.Background(), in, nil)
	if !reflect.DeepEqual(out, in) || evaluated != nil || len(logged) == 0 {
		t.Fatalf("out %+v evaluated %v logged %v", out, evaluated, logged)
	}
}

func TestFindingFilterAppliesConfirmedFacts(t *testing.T) {
	src := &fakeConfirmed{facts: []Fact{
		confirmed(7, TypeTestFixture, KindSchema, "test_memory_*", nil)}}
	f := NewFindingFilter(src, nil)
	current, previous := fixtureSnapshot(), fixtureSnapshot()
	excluded := f.ExcludeSnapshot(context.Background(), current, previous)
	if len(excluded) != 2 || len(current.Tables) != 2 || len(previous.Tables) != 2 {
		t.Fatalf("excluded %v; tables %d/%d", excluded, len(current.Tables),
			len(previous.Tables))
	}
	out, evaluated := f.ApplyFindings(context.Background(), nil, excluded)
	if len(out) != 1 || out[0].Category != CategoryTestFixtureCleanup ||
		len(evaluated) != 1 {
		t.Fatalf("out %+v evaluated %v", out, evaluated)
	}
}
