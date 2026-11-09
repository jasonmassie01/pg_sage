package facts

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/policy"
)

// facts-10 review: resolving refs against the catalog may only add
// matches. PostgreSQL runs ALTER INDEX <table> RENAME and ALTER TABLE
// <index> ..., so a ref written as one kind can be the other; it must keep
// matching the patterns of the kind it was written as and gain the
// catalog's.

// No concurrent access tests: applyRelkind and Bind are pure functions of
// their arguments and add no shared state.

func TestApplyRelkindNeverDropsTheWrittenKind(t *testing.T) {
	none, root := [2]string{}, [2]string{"app", "events"}
	table := [2]string{"app", "orders"}
	cases := []struct {
		name    string
		in      ObjectRef
		relkind string
		want    ObjectRef
	}{
		{"unknown kind takes the table's", ObjectRef{Kind: KindRelation, Name: "orders"}, "r",
			ObjectRef{Kind: KindTable, Name: "orders"}},
		{"unknown kind takes the index's", ObjectRef{Kind: KindRelation, Name: "i"}, "i",
			ObjectRef{Kind: KindIndex, Name: "i", TableSchema: "app", TableName: "orders"}},
		{"index named as a table is both", ObjectRef{Kind: KindTable, Name: "i"}, "I",
			ObjectRef{Kind: KindRelation, Name: "i", TableSchema: "app", TableName: "orders"}},
		{"table named as an index is both", ObjectRef{Kind: KindIndex, Name: "orders"}, "p",
			ObjectRef{Kind: KindRelation, Name: "orders"}},
		{"a sequence changes nothing", ObjectRef{Kind: KindTable, Name: "s"}, "S",
			ObjectRef{Kind: KindTable, Name: "s"}},
		{"an index keeps its written table", ObjectRef{Kind: KindIndex, Name: "i",
			TableSchema: "x", TableName: "y"}, "i",
			ObjectRef{Kind: KindIndex, Name: "i", TableSchema: "x", TableName: "y"}},
	}
	for _, c := range cases {
		got := c.in
		applyRelkind(&got, c.relkind, table, none)
		if got != c.want {
			t.Errorf("%s: %#v, want %#v", c.name, got, c.want)
		}
	}
	got := ObjectRef{Kind: KindIndex, Name: "orders"}
	applyRelkind(&got, "r", table, root)
	if got.Kind != KindRelation || got.RootSchema != "app" || got.RootName != "events" {
		t.Fatalf("a partition named as an index keeps its root: %#v", got)
	}
}

func TestPatternsKeepMatchingRefsWrittenAsTheOtherKind(t *testing.T) {
	for _, c := range []struct {
		written, catalog Kind
		relkind          string
	}{{KindIndex, KindTable, "r"}, {KindTable, KindIndex, "i"}} {
		ref := ObjectRef{Kind: c.written, Schema: "app", Name: "orders"}
		before := map[Kind]bool{}
		for _, k := range []Kind{KindTable, KindIndex, KindSchema} {
			before[k] = mustPattern(t, k, patternFor(k)).Matches(ref)
		}
		applyRelkind(&ref, c.relkind, [2]string{"app", "orders"}, [2]string{})
		for k, was := range before {
			if was && !mustPattern(t, k, patternFor(k)).Matches(ref) {
				t.Errorf("%s ref resolved as relkind %s lost its %s match: %#v",
					c.written, c.relkind, k, ref)
			}
		}
		if !mustPattern(t, c.catalog, "app.orders").Matches(ref) {
			t.Errorf("%s ref resolved as relkind %s does not match a %s pattern: %#v",
				c.written, c.relkind, c.catalog, ref)
		}
	}
}

func patternFor(k Kind) string {
	if k == KindSchema {
		return "app"
	}
	return "app.orders"
}

// dropCatalog breaks the one-ref-per-ref contract.
type dropCatalog struct{}

func (dropCatalog) ResolveCatalog(_ context.Context, refs []ObjectRef) ([]ObjectRef, error) {
	return refs[:len(refs)-1], nil
}

func TestBinderFailsClosedWhenTheResolverDropsRefs(t *testing.T) {
	src := &fakeConfirmed{facts: []Fact{
		confirmed(31, TypeAppMigrations, KindTable, "app.events", nil)}}
	b := NewBinder(src, dropCatalog{}, func() time.Time { return bindNow })
	got, err := b.Bind(context.Background(), setAutovacuum(
		"ALTER TABLE app.events SET (fillfactor=90)"))
	if err == nil || !strings.Contains(err.Error(), "returned 0 refs for 1") || got != nil {
		t.Fatalf("a short resolver answer must fail the gate closed: %+v %v", got, err)
	}
}

// On the base, ALTER INDEX of a table was bound by the table's fact; it
// still is, and ALTER TABLE of an index is bound by the index's fact.
func TestBinderBindsRefsWrittenAsTheOtherKindOnLiveDB(t *testing.T) {
	pool, ctx := livePool(t)
	s := createPartitionedEvents(t, ctx, pool)
	q := pgx.Identifier{s}.Sanitize()
	store := NewStore(pool)
	plain := declareFact(t, ctx, store, TypeAppMigrations, q+".plain", nil)
	parent := declareFact(t, ctx, store, TypeAppMigrations, q+".events", nil)
	p := operatorProposal(TypeAppMigrations, KindIndex, q+".events_p01_ts_idx")
	idx, err := store.Declare(ctx, p, "op", "")
	if err != nil {
		t.Fatal(err)
	}
	b := NewBinder(store, NewCatalogResolver(pool), nil)
	for _, c := range []struct {
		sql  string
		want []int64
	}{
		{"ALTER INDEX " + q + ".plain RENAME TO plain_old", []int64{plain.ID}},
		{"ALTER INDEX " + q + ".events_p01 RENAME TO p01_old", []int64{parent.ID}},
		{"ALTER TABLE " + q + ".events_p01_ts_idx SET (fillfactor=80)",
			[]int64{parent.ID, idx.ID}},
	} {
		got, err := b.Bind(ctx, policy.ActionRequest{SQL: c.sql})
		if err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		var ids []int64
		for _, g := range got {
			ids = append(ids, g.FactID)
		}
		if !reflect.DeepEqual(ids, c.want) {
			t.Errorf("%s: bound %v (%+v), want %v", c.sql, ids, got, c.want)
		}
	}
}
