package optimizer

import (
	"strings"
	"testing"
)

// indexes-12 review: a statement over several relations attributes its
// filters to every one of them, and quoted identifiers keep their case.
// The floor must read only the filters on its own table's columns, by
// their exact names. No concurrency test here: the parser is a pure
// function of its arguments.

func floorEq(t *testing.T, stmt, table string) []string {
	t.Helper()
	f, _ := parseFloorFilters(stmt, table)
	return f.eq
}

func TestParseFloorFilters_JoinCountsOnlyTheTablesQualifiedColumns(t *testing.T) {
	for _, tc := range []struct{ stmt, want string }{
		{"SELECT o.id FROM public.orders o JOIN public.customers c ON c.id = o.customer_id " +
			"WHERE c.region = $1", ""},
		{"SELECT o.id FROM public.orders o JOIN public.customers c ON c.id = o.customer_id " +
			"WHERE o.customer_id = $1 AND c.region = $2", "customer_id"},
		{"SELECT 1 FROM public.orders JOIN public.customers USING (customer_id) " +
			"WHERE region = $1", ""},
		{"SELECT 1 FROM public.orders AS o, public.customers c WHERE o.region = $1 " +
			"AND c.status = $2", "region"},
		{"SELECT 1 FROM public.orders, public.customers WHERE orders.status = $1 " +
			"AND customers.region = $2", "status"},
		{"DELETE FROM public.orders o USING public.customers c WHERE o.customer_id = c.id " +
			"AND o.region = $1", "region"},
		{"UPDATE public.orders o SET status = $1 FROM public.customers c " +
			"WHERE c.id = o.customer_id AND c.region = $2", ""},
	} {
		if got := strings.Join(floorEq(t, tc.stmt, "orders"), ","); got != tc.want {
			t.Errorf("%q: equality filters %q, want %q", tc.stmt, got, tc.want)
		}
	}
}

func TestParseFloorFilters_SingleRelationCountsEveryFilter(t *testing.T) {
	for _, tc := range []struct{ stmt, want string }{
		{"UPDATE public.orders SET region = $1, status = $2 WHERE customer_id = $3",
			"customer_id"},
		{"SELECT extract(year FROM created_at), count(*) FROM public.orders o " +
			"WHERE o.customer_id = $1 AND region = $2", "customer_id,region"},
		{"SELECT id FROM orders WHERE created_at >= $1 AND customer_id != $2", ""},
	} {
		if got := strings.Join(floorEq(t, tc.stmt, "orders"), ","); got != tc.want {
			t.Errorf("%q: equality filters %q, want %q", tc.stmt, got, tc.want)
		}
	}
	f, ok := parseFloorFilters("SELECT id FROM orders WHERE created_at >= $1", "orders")
	if !ok || strings.Join(f.rng, ",") != "created_at" || len(f.eq) != 0 {
		t.Fatalf("range filter = %+v ok %v", f, ok)
	}
}

// A quoted mixed-case column (ORMs create them) keeps its case in the
// DDL; an unquoted name folds to lower case and matches no such column.
func TestFloorCandidates_QuotedMixedCaseColumnKeepsItsName(t *testing.T) {
	tc := TableContext{Schema: "public", Table: "Orders", LiveTuples: 100000,
		ColumnsKnown: true, Columns: []ColumnInfo{{Name: "id"}, {Name: "customerId"}},
		ColStats: []ColStat{{Column: "customerId", NDistinct: -0.5}},
		Queries: []QueryInfo{{QueryID: 1, Calls: 500,
			Text: `SELECT id FROM public."Orders" WHERE "customerId" = $1`}}}
	recs := floorCandidates(tc)
	if len(recs) != 1 || !strings.Contains(recs[0].DDL, `ON public."Orders" ("customerId")`) {
		t.Fatalf("candidates = %+v", recs)
	}
	tc.Queries[0].Text = `SELECT id FROM public."Orders" WHERE customerId = $1`
	if recs := floorCandidates(tc); len(recs) != 0 {
		t.Fatalf("customerId unquoted is customerid, no column of the table: %+v", recs)
	}
}
