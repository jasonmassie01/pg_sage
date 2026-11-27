package optimizer

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// indexes-04: TableContext attributed statements with its own word
// scanner, which put every unqualified name in public, lower-cased quoted
// identifiers and read only the first relation after FROM.

func tcTables(names ...[2]string) []collector.TableStats {
	out := make([]collector.TableStats, 0, len(names))
	for _, n := range names {
		out = append(out, collector.TableStats{SchemaName: n[0], RelName: n[1],
			NLiveTup: 1000, SeqScan: 5})
	}
	return out
}

func tcQueries(texts ...string) []collector.QueryStats {
	out := make([]collector.QueryStats, 0, len(texts))
	for i, q := range texts {
		out = append(out, collector.QueryStats{QueryID: int64(i + 1), Query: q, Calls: 50,
			MeanExecTime: 2, TotalExecTime: 100})
	}
	return out
}

// contextIDs builds table's context with no database and returns its
// statements' queryids, sorted.
func contextIDs(t *testing.T, snap *collector.Snapshot, table string) []int64 {
	t.Helper()
	o := New(nil, &config.OptimizerConfig{MinQueryCalls: 1}, func(string, string, ...any) {})
	tc, ok, err := o.TableContext(context.Background(), snap, table)
	if err != nil || !ok {
		t.Fatalf("TableContext(%s): ok %v err %v", table, ok, err)
	}
	ids := contextQueryIDs(tc)
	slices.Sort(ids)
	return ids
}

func assertIDs(t *testing.T, snap *collector.Snapshot, table string, want ...int64) {
	t.Helper()
	got := contextIDs(t, snap, table)
	if !slices.Equal(got, want) {
		t.Fatalf("%s statements = %v, want %v", table, got, want)
	}
}

// Happy path (failed before: the statement went to public.orders, which
// does not exist, and app.orders had no statements).
func TestTableContext_UnqualifiedStatementLandsInItsOnlySchema(t *testing.T) {
	snap := &collector.Snapshot{Tables: tcTables([2]string{"app", "orders"}),
		Queries: tcQueries("SELECT id FROM orders WHERE customer_id = $1")}
	assertIDs(t, snap, "app.orders", 1)
}

// Boundary: a qualified statement goes to its schema even when public has
// the same name; an unqualified one goes to public (the documented rule).
func TestTableContext_QualifiedAndAmbiguousNames(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: tcTables([2]string{"public", "orders"}, [2]string{"app", "orders"}),
		Queries: tcQueries("SELECT * FROM app.orders WHERE id = $1",
			"SELECT * FROM public.orders WHERE id = $1",
			"SELECT * FROM orders WHERE status = $1")}
	assertIDs(t, snap, "app.orders", 1)
	assertIDs(t, snap, "public.orders", 2, 3)
}

// Invalid input: quoted mixed-case names (failed before: app"."items).
func TestTableContext_QuotedMixedCaseTable(t *testing.T) {
	snap := &collector.Snapshot{Tables: tcTables([2]string{"App", "Items"}),
		Queries: tcQueries(`SELECT * FROM "App"."Items" WHERE "Id" = $1`)}
	assertIDs(t, snap, `"App"."Items"`, 1)
}

// Invalid input: the second relation of a FROM list (failed before) and a
// relation after ONLY (failed before: read as a table named "only").
func TestTableContext_FromListAndOnly(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: tcTables([2]string{"public", "a"}, [2]string{"public", "b"},
			[2]string{"public", "t"}),
		Queries: tcQueries("SELECT * FROM a, b WHERE a.id = b.id",
			"SELECT * FROM ONLY t WHERE x = $1")}
	assertIDs(t, snap, "public.a", 1)
	assertIDs(t, snap, "public.b", 1)
	assertIDs(t, snap, "public.t", 2)
}

// Invalid input: a CTE name is no table, even one named like a real table,
// and a comment or literal naming a table is no reference to it (the
// comment failed before).
func TestTableContext_CTEsLiteralsAndComments(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: tcTables([2]string{"app", "orders"}, [2]string{"public", "x"},
			[2]string{"public", "recent"}),
		Queries: tcQueries(
			"WITH recent AS (SELECT * FROM orders WHERE ts > $1) SELECT * FROM recent",
			"SELECT 'FROM x' AS v FROM orders -- JOIN x\n WHERE id = $1")}
	assertIDs(t, snap, "app.orders", 1, 2)
	assertIDs(t, snap, "public.x")
	assertIDs(t, snap, "public.recent")
}

// Nil/empty: an empty statement, an unknown relation and an empty snapshot
// attribute nothing; a nil snapshot is an error.
func TestTableContext_EmptyAndUnknown(t *testing.T) {
	snap := &collector.Snapshot{Tables: tcTables([2]string{"public", "orders"}),
		Queries: tcQueries("", "SELECT * FROM missing WHERE id = $1", "SELECT 1")}
	assertIDs(t, snap, "public.orders")
	o := New(nil, &config.OptimizerConfig{MinQueryCalls: 1}, func(string, string, ...any) {})
	if _, ok, err := o.TableContext(context.Background(), &collector.Snapshot{},
		"public.orders"); ok || err != nil {
		t.Fatalf("empty snapshot: ok %v err %v, want not found and no error", ok, err)
	}
	if _, ok, err := o.TableContext(context.Background(), nil, "public.orders"); ok ||
		err == nil {
		t.Fatalf("nil snapshot: ok %v err %v, want an error", ok, err)
	}
}

// Partitions: an unqualified statement on a child in a non-public schema
// merges into the parent's context (failed before).
func TestTableContext_ChildStatementsMergeIntoParent(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: tcTables([2]string{"app", "events"}, [2]string{"app", "events_2024"}),
		Queries: tcQueries("SELECT * FROM events WHERE id = $1",
			"SELECT * FROM events_2024 WHERE ts > $1"),
		Partitions: []collector.PartitionInfo{{ParentSchema: "app", ParentTable: "events",
			ChildSchema: "app", ChildTable: "events_2024"}}}
	assertIDs(t, snap, "app.events", 1, 2)
}

// Concurrent access: contexts of two schemas built in parallel from one
// snapshot each hold only their own statements. -race is unavailable on
// this machine; the assertions still catch crossed results.
func TestTableContext_ParallelSchemasStayApart(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: tcTables([2]string{"app", "orders"}, [2]string{"public", "items"}),
		Queries: tcQueries("SELECT * FROM orders WHERE id = $1",
			"SELECT * FROM items WHERE id = $1")}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			table, want := "app.orders", int64(1)
			if i%2 == 1 {
				table, want = "public.items", 2
			}
			o := New(nil, &config.OptimizerConfig{MinQueryCalls: 1},
				func(string, string, ...any) {})
			tc, ok, err := o.TableContext(context.Background(), snap, table)
			ids := contextQueryIDs(tc)
			if err != nil || !ok || len(ids) != 1 || ids[0] != want {
				t.Errorf("%s: ids %v ok %v err %v, want [%d]", table, ids, ok, err, want)
			}
		}()
	}
	wg.Wait()
}

// No state-transition tests: the table context has no modes or states.
