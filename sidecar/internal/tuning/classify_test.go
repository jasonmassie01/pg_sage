package tuning

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/facts"
)

// Workload classification (owner decision 2): which statements are app,
// tenant, test, diagnostic or pg_sage's own, and which tables confirmed
// facts bind. Deterministic; the model only ever sees the result.

func classifySnap(qs ...collector.QueryStats) *collector.Snapshot {
	return snapAt(t0, qs, []collector.TableStats{
		table("public", "orders", 1000, 0),
		table("app", "invoices", 1000, 0),
		table("test_abc", "orders", 10, 0),
		table("fixtures_one", "items", 10, 0),
	}, nil)
}

func TestClassifyWorkload_AppStatementResolvesQualifiedTables(t *testing.T) {
	snap := classifySnap(stmt(1, "SELECT * FROM public.orders o JOIN app.invoices i "+
		"ON i.order_id = o.id WHERE o.id = $1", 10, 100))
	w := ClassifyWorkload(snap, nil, t0)
	got := w.Statements[1]
	if got.Class != ClassApp {
		t.Fatalf("class = %q, want app (reason %q)", got.Class, got.Reason)
	}
	want := []string{"app.invoices", "public.orders"}
	if !reflect.DeepEqual(got.Tables, want) {
		t.Fatalf("tables = %v, want %v (sorted, deduplicated)", got.Tables, want)
	}
	if !w.IsWorkload(1) {
		t.Fatal("an app statement must be workload")
	}
	if w.Counts[ClassApp] != 1 {
		t.Fatalf("counts = %v", w.Counts)
	}
}

func TestClassifyWorkload_DiagnosticAndOwnStatementsAreNeverWorkload(t *testing.T) {
	snap := classifySnap(
		stmt(1, "EXPLAIN (ANALYZE, BUFFERS) SELECT * FROM public.orders", 1, 9000),
		stmt(2, "VACUUM public.orders", 3, 100),
		stmt(3, "/* pg_sage */ SELECT count(*) FROM sage.findings", 50, 10),
		stmt(4, "COPY public.orders TO STDOUT", 1, 5000),
	)
	w := ClassifyWorkload(snap, nil, t0)
	for id, want := range map[int64]StatementClass{1: ClassDiagnostic, 2: ClassDiagnostic,
		3: ClassSage, 4: ClassDiagnostic} {
		got := w.Statements[id]
		if got.Class != want {
			t.Errorf("statement %d: class %q, want %q", id, got.Class, want)
		}
		if got.Reason == "" {
			t.Errorf("statement %d: no reason recorded", id)
		}
		if w.IsWorkload(id) {
			t.Errorf("statement %d must not be workload", id)
		}
	}
	if w.Counts[ClassDiagnostic] != 3 || w.Counts[ClassSage] != 1 {
		t.Fatalf("counts = %v", w.Counts)
	}
}

func TestClassifyWorkload_TestSchemaByNameIsTest(t *testing.T) {
	snap := classifySnap(stmt(1, "SELECT * FROM test_abc.orders WHERE id = $1", 99, 900))
	got := ClassifyWorkload(snap, nil, t0).Statements[1]
	if got.Class != ClassTest {
		t.Fatalf("class = %q, want test", got.Class)
	}
	if !strings.Contains(got.Reason, "test_abc") {
		t.Fatalf("reason %q must name the schema", got.Reason)
	}
}

func TestClassifyWorkload_ConfirmedFixtureFactMakesTest(t *testing.T) {
	snap := classifySnap(stmt(1, "SELECT * FROM fixtures_one.items", 9, 90))
	fixture := confirmedFact(7, facts.TypeTestFixture, facts.KindSchema, "fixtures_*", nil)
	got := ClassifyWorkload(snap, []facts.Fact{fixture}, t0).Statements[1]
	if got.Class != ClassTest || !strings.Contains(got.Reason, "#7") {
		t.Fatalf("class %q reason %q: want test citing fact #7", got.Class, got.Reason)
	}
	proposed := fixture
	proposed.Status = facts.StatusProposed
	got = ClassifyWorkload(snap, []facts.Fact{proposed}, t0).Statements[1]
	if got.Class != ClassApp {
		t.Fatalf("a proposed (unconfirmed) fact must not classify: got %q", got.Class)
	}
}

func TestClassifyWorkload_RejectedFixtureFactOverridesTheNameHeuristic(t *testing.T) {
	snap := classifySnap(stmt(1, "SELECT * FROM test_abc.orders", 9, 90))
	rejected := confirmedFact(8, facts.TypeTestFixture, facts.KindSchema, "test_abc", nil)
	rejected.Status = facts.StatusRejected
	got := ClassifyWorkload(snap, []facts.Fact{rejected}, t0).Statements[1]
	if got.Class != ClassApp {
		t.Fatalf("the operator rejected 'test_abc is a fixture': class %q, want app", got.Class)
	}
}

func tenantSnap(n int, q collector.QueryStats) *collector.Snapshot {
	var ts []collector.TableStats
	for i := 0; i < n; i++ {
		s := fmt.Sprintf("tenant_%06d", i+100)
		ts = append(ts, table(s, "orders", 100, 0), table(s, "items", 100, 0))
	}
	return snapAt(t0, []collector.QueryStats{q}, ts, nil)
}

func TestClassifyWorkload_TenantFamily(t *testing.T) {
	q := stmt(1, "SELECT * FROM tenant_000102.orders WHERE id = $1", 50, 500)
	got := ClassifyWorkload(tenantSnap(5, q), nil, t0).Statements[1]
	if got.Class != ClassTenant || got.Family == "" {
		t.Fatalf("five copies of one shape are a tenant family: %+v", got)
	}
	if !ClassifyWorkload(tenantSnap(5, q), nil, t0).IsWorkload(1) {
		t.Fatal("tenant statements are workload")
	}
	// Boundary: four copies are not a family.
	got = ClassifyWorkload(tenantSnap(4, q), nil, t0).Statements[1]
	if got.Class != ClassApp || got.Family != "" {
		t.Fatalf("four copies are not a family: %+v", got)
	}
}

func TestClassifyWorkload_UnqualifiedNames(t *testing.T) {
	snap := snapAt(t0, []collector.QueryStats{
		stmt(1, "SELECT * FROM invoices WHERE id = $1", 1, 1),
		stmt(2, "SELECT * FROM orders", 1, 1),
		stmt(3, "SELECT * FROM shared_name", 1, 1),
		stmt(4, "UPDATE orders SET status = $1 WHERE id = $2", 1, 1),
	}, []collector.TableStats{table("app", "invoices", 1, 0), table("public", "orders", 1, 0),
		table("app", "orders", 1, 0), table("s1", "shared_name", 1, 0),
		table("s2", "shared_name", 1, 0)}, nil)
	w := ClassifyWorkload(snap, nil, t0)
	if got := w.Statements[1].Tables; !reflect.DeepEqual(got, []string{"app.invoices"}) {
		t.Errorf("unique name resolves to its schema: %v", got)
	}
	if got := w.Statements[2].Tables; !reflect.DeepEqual(got, []string{"public.orders"}) {
		t.Errorf("ambiguous name present in public resolves to public: %v", got)
	}
	if got := w.Statements[3].Tables; len(got) != 0 {
		t.Errorf("ambiguous name outside public stays unresolved: %v", got)
	}
	if got := w.Statements[4].Tables; !reflect.DeepEqual(got, []string{"public.orders"}) {
		t.Errorf("UPDATE target resolves: %v", got)
	}
}

func TestClassifyWorkload_QuotedAndMixedCaseIdentifiers(t *testing.T) {
	snap := snapAt(t0, []collector.QueryStats{
		stmt(1, `SELECT * FROM "Sales"."Orders" WHERE id = $1`, 1, 1),
		stmt(2, `SELECT * FROM Public.ORDERS`, 1, 1),
	}, []collector.TableStats{table("Sales", "Orders", 1, 0), table("public", "orders", 1, 0)},
		nil)
	w := ClassifyWorkload(snap, nil, t0)
	if got := w.Statements[1].Tables; !reflect.DeepEqual(got, []string{`"Sales"."Orders"`}) {
		t.Errorf("quoted identifiers keep their case and quotes: %v", got)
	}
	if got := w.Statements[2].Tables; !reflect.DeepEqual(got, []string{"public.orders"}) {
		t.Errorf("unquoted identifiers fold to lower case: %v", got)
	}
}

func TestClassifyWorkload_TableRoutesFromConfirmedFacts(t *testing.T) {
	snap := classifySnap(stmt(1, "SELECT * FROM public.orders", 1, 1),
		stmt(2, "SELECT * FROM app.invoices", 1, 1))
	confirmed := []facts.Fact{
		confirmedFact(12, facts.TypeAppMigrations, facts.KindTable, "public.orders", nil),
		confirmedFact(13, facts.TypeAppendOnly, facts.KindTable, "app.invoices", nil),
	}
	w := ClassifyWorkload(snap, confirmed, t0)
	orders := w.Tables["public.orders"]
	if !slices.Contains(orders.Routes, facts.RouteSourceFix) ||
		!slices.Contains(orders.FactIDs, 12) {
		t.Fatalf("migration-managed table: %+v", orders)
	}
	invoices := w.Tables["app.invoices"]
	if !slices.Contains(invoices.Routes, facts.RouteKeep) || slices.Contains(invoices.Routes,
		facts.RouteSourceFix) {
		t.Fatalf("append-only table keeps its indexes, is not migration-managed: %+v",
			invoices)
	}
	if got := w.Tables["test_abc.orders"]; got.Class != ClassTest {
		t.Fatalf("a table in a test schema is test: %+v", got)
	}
}

func TestClassifyWorkload_NilAndEmpty(t *testing.T) {
	w := ClassifyWorkload(nil, nil, t0)
	if len(w.Statements) != 0 || len(w.Tables) != 0 || w.IsWorkload(1) {
		t.Fatalf("nil snapshot classifies nothing: %+v", w)
	}
	w = ClassifyWorkload(&collector.Snapshot{}, nil, t0)
	if len(w.Statements) != 0 {
		t.Fatalf("empty snapshot classifies nothing: %+v", w)
	}
	if w.IsWorkload(0) {
		t.Fatal("an unknown queryid is not workload")
	}
}

// No concurrent-access test: ClassifyWorkload is a pure function of its
// arguments and holds no state.
