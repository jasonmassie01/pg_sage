package facts

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// evid-03b review: REINDEX TABLE, VACUUM FULL and CLUSTER rebuild every
// index of their table though their SQL names only the table, so a
// do_not_touch fact on one of those indexes holds them. Statements that do
// not rebuild indexes, and requests without an index do_not_touch fact,
// never list the table's indexes.
// No concurrent access tests: the expansion reads one facts snapshot per
// Bind, pinned in do_not_touch_test.go.

// indexLister is a resolver that lists fixed indexes per table.
type indexLister struct {
	indexes map[string][]string // "schema.table" -> index names in its schema
	err     error
	calls   int
}

func (l *indexLister) ResolveCatalog(_ context.Context, refs []ObjectRef) ([]ObjectRef,
	error) {
	return refs, nil
}

func (l *indexLister) TableIndexes(_ context.Context, tables []ObjectRef) ([]ObjectRef,
	error) {
	l.calls++
	if l.err != nil {
		return nil, l.err
	}
	var out []ObjectRef
	for _, t := range tables {
		for _, name := range l.indexes[t.Schema+"."+t.Name] {
			out = append(out, ObjectRef{Kind: KindIndex, Schema: t.Schema, Name: name,
				TableSchema: t.Schema, TableName: t.Name})
		}
	}
	return out, nil
}

type fixedFacts []Fact

func (f fixedFacts) Confirmed(context.Context) ([]Fact, error) { return f, nil }

func indexBinder(l *indexLister, facts ...Fact) *Binder {
	return NewBinder(fixedFacts(facts), l, func() time.Time { return bindNow })
}

func bindOperator(t *testing.T, b *Binder, action, sql string) []policy.FactBinding {
	t.Helper()
	got, err := b.Bind(context.Background(), policy.ActionRequest{SQL: sql,
		OperatorApproved: true, Contract: &policy.ActionContract{ActionType: action}})
	if err != nil {
		t.Fatalf("bind %q: %v", sql, err)
	}
	return got
}

func TestDoNotTouchIndexHoldsItsTablesRebuilds(t *testing.T) {
	l := &indexLister{indexes: map[string][]string{
		"public.orders": {"idx_orders_id", "idx_orders_status"}}}
	b := indexBinder(l, doNotTouch(4, KindIndex, "public.idx_orders_status"))
	for _, sql := range []string{"REINDEX TABLE CONCURRENTLY public.orders",
		"REINDEX (VERBOSE) TABLE public.orders", "VACUUM (FULL, ANALYZE) public.orders",
		"VACUUM FULL public.orders", "CLUSTER public.orders USING idx_orders_id"} {
		got := bindOperator(t, b, "reindex_table", sql)
		if len(got) != 1 || got[0].FactID != 4 || got[0].Route != "hold" ||
			got[0].Object != "public.idx_orders_status" {
			t.Errorf("%s: bindings %+v, want fact 4 held on its index", sql, got)
		}
	}
	for _, sql := range []string{"VACUUM (ANALYZE) public.orders", "ANALYZE public.orders",
		"REINDEX INDEX CONCURRENTLY public.idx_orders_id",
		"REINDEX TABLE CONCURRENTLY public.customers"} {
		if got := bindOperator(t, b, "reindex_table", sql); got != nil {
			t.Errorf("%s: bound %+v, want nothing", sql, got)
		}
	}
	if l.calls != 6 { // the five rebuilds and the other table's REINDEX TABLE
		t.Fatalf("indexes listed %d times, want 6 (only for rebuilds)", l.calls)
	}
}

func TestDoNotTouchIndexExpansionOnlyWithAnIndexFact(t *testing.T) {
	l := &indexLister{indexes: map[string][]string{"public.orders": {"idx_orders_status"}}}
	b := indexBinder(l, doNotTouch(1, KindTable, "public.customers"),
		confirmed(2, TypeAppMigrations, KindIndex, "public.idx_orders_status", nil))
	if got := bindOperator(t, b, "reindex_table",
		"REINDEX TABLE CONCURRENTLY public.orders"); got != nil {
		t.Fatalf("bound %+v without an index do_not_touch fact", got)
	}
	if l.calls != 0 {
		t.Fatalf("indexes listed %d times without an index do_not_touch fact", l.calls)
	}
	// The rollback of pg_sage's own change is never bound, rebuild or not.
	b = indexBinder(l, doNotTouch(4, KindIndex, "public.idx_orders_status"))
	got, err := b.Bind(context.Background(), policy.ActionRequest{
		SQL: "REINDEX TABLE public.orders", OperatorApproved: true,
		Contract: &policy.ActionContract{ActionType: "revert_created_index"}})
	if err != nil || got != nil {
		t.Fatalf("a rollback was bound: %+v %v", got, err)
	}
}

// A failed index listing is an error (the gate fails closed), never
// "nothing binds".
func TestDoNotTouchIndexListingFailureIsAnError(t *testing.T) {
	l := &indexLister{err: errors.New("connection refused")}
	b := indexBinder(l, doNotTouch(4, KindIndex, "public.idx_orders_status"))
	_, err := b.Bind(context.Background(), policy.ActionRequest{
		SQL: "VACUUM FULL public.orders", OperatorApproved: true,
		Contract: &policy.ActionContract{ActionType: "vacuum_full"}})
	if err == nil || !strings.Contains(err.Error(), "connection refused") ||
		!strings.Contains(err.Error(), "indexes") {
		t.Fatalf("err = %v, want the index listing failure", err)
	}
}
