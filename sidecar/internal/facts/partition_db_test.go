package facts

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/policy"
)

// facts-10 on real PostgreSQL (pg_partition_root exists from PG 12): a
// table fact on a partitioned table binds work on its partitions, their
// sub-partitions and their indexes; legacy inheritance children are not
// partitions and are not covered.

// createPartitionedEvents makes <schema>.events partitioned by range, with
// events_p01 (and its index), events_p02 sub-partitioned by list into
// events_p02_us, a quoted "Events_P03", a legacy-inheritance legacy_c1 of
// legacy, and a plain table.
func createPartitionedEvents(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	schemaName := uniqueName("parts_")
	q := pgx.Identifier{schemaName}.Sanitize()
	execAll(t, ctx, pool, "CREATE SCHEMA "+q,
		"CREATE TABLE "+q+".events (id int, ts timestamptz NOT NULL, region text) "+
			"PARTITION BY RANGE (ts)",
		"CREATE TABLE "+q+".events_p01 PARTITION OF "+q+".events "+
			"FOR VALUES FROM ('2026-11-01') TO ('2026-12-01')",
		"CREATE INDEX events_p01_ts_idx ON "+q+".events_p01 (ts)",
		"CREATE TABLE "+q+".events_p02 PARTITION OF "+q+".events "+
			"FOR VALUES FROM ('2026-12-01') TO ('2027-01-01') PARTITION BY LIST (region)",
		"CREATE TABLE "+q+".events_p02_us PARTITION OF "+q+".events_p02 FOR VALUES IN ('us')",
		"CREATE TABLE "+q+`."Events_P03" PARTITION OF `+q+".events "+
			"FOR VALUES FROM ('2027-01-01') TO ('2027-02-01')",
		"CREATE TABLE "+q+".legacy (id int)",
		"CREATE TABLE "+q+".legacy_c1 () INHERITS ("+q+".legacy)",
		"CREATE TABLE "+q+".plain (id int)")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+q+" CASCADE")
	})
	return schemaName
}

func TestCatalogResolverFindsPartitionRootsOnLiveDB(t *testing.T) {
	pool, ctx := livePool(t)
	s := createPartitionedEvents(t, ctx, pool)
	rel := func(kind Kind, name string) ObjectRef {
		return ObjectRef{Kind: kind, Schema: s, Name: name}
	}
	refs := []ObjectRef{rel(KindTable, "events_p01"), rel(KindRelation, "events_p02_us"),
		rel(KindTable, "Events_P03"), rel(KindIndex, "events_p01_ts_idx"),
		rel(KindTable, "events"), rel(KindTable, "legacy_c1"), rel(KindRelation, "plain"),
		rel(KindTable, "missing"), {Kind: KindTable, Name: "events_p01"}}
	got, err := NewCatalogResolver(pool).ResolveCatalog(ctx, refs)
	if err != nil {
		t.Fatal(err)
	}
	rooted := func(r ObjectRef) ObjectRef {
		r.RootSchema, r.RootName = s, "events"
		return r
	}
	idx := rooted(rel(KindIndex, "events_p01_ts_idx"))
	idx.TableSchema, idx.TableName = s, "events_p01"
	want := []ObjectRef{rooted(rel(KindTable, "events_p01")),
		rooted(rel(KindTable, "events_p02_us")), // a sub-partition: the top root
		rooted(rel(KindTable, "Events_P03")), idx,
		rel(KindTable, "events"),    // the root has no root of its own
		rel(KindTable, "legacy_c1"), // legacy inheritance: not a partition
		rel(KindTable, "plain"), rel(KindTable, "missing"),
		{Kind: KindTable, Name: "events_p01"}} // unqualified: as written
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved\n got %+v\nwant %+v", got, want)
	}
}

// outsideWindow is a maintenance window that does not contain now.
func outsideWindow() string {
	h := (time.Now().UTC().Hour() + 12) % 24
	return fmt.Sprintf("daily %02d:00-%02d:30 UTC", h, h)
}

func declareFact(t *testing.T, ctx context.Context, s *Store, typ Type, subject string,
	value map[string]string) Fact {
	t.Helper()
	p := operatorProposal(typ, KindTable, subject)
	p.Value = value
	f, err := s.Declare(ctx, p, "op", "")
	if err != nil {
		t.Fatalf("declare %s on %s: %v", typ, subject, err)
	}
	return f
}

func TestBinderBindsPartitionsToParentFactsOnLiveDB(t *testing.T) {
	pool, ctx := livePool(t)
	schemaName := createPartitionedEvents(t, ctx, pool)
	q := pgx.Identifier{schemaName}.Sanitize()
	store := NewStore(pool)
	app := declareFact(t, ctx, store, TypeAppMigrations, q+".events", nil)
	keep := declareFact(t, ctx, store, TypeAppendOnly, q+".events", nil)
	window := declareFact(t, ctx, store, TypeTableWindow, q+".events",
		map[string]string{"kind": "maintenance", "window": outsideWindow()})
	legacy := declareFact(t, ctx, store, TypeAppendOnly, q+".legacy", nil)
	b := NewBinder(store, NewCatalogResolver(pool), nil)
	drop := policy.ActionRequest{SQL: "DROP INDEX " + q + ".events_p01_ts_idx",
		Contract: &policy.ActionContract{ActionType: "drop_unused_index",
			RiskTier: policy.RiskSafe}}
	cases := []struct {
		req  policy.ActionRequest
		want []int64
	}{
		// CHECK-01
		{setAutovacuum("ALTER TABLE " + q + ".events_p01 SET " +
			"(autovacuum_vacuum_scale_factor=0.01)"), []int64{app.ID, window.ID}},
		{policy.ActionRequest{SQL: "VACUUM (FULL) " + q + ".events_p01"},
			[]int64{keep.ID, window.ID}},
		{drop, []int64{app.ID, keep.ID, window.ID}},
		{policy.ActionRequest{SQL: "VACUUM " + q + ".events_p01"}, []int64{window.ID}},
		{policy.ActionRequest{SQL: "VACUUM (FULL) " + q + ".events_p02_us"},
			[]int64{keep.ID, window.ID}},
		{setAutovacuum("ALTER TABLE " + q + `."Events_P03" SET (fillfactor=90)`),
			[]int64{app.ID, window.ID}},
		{policy.ActionRequest{SQL: "VACUUM (FULL) " + q + ".legacy_c1"}, nil},
		{policy.ActionRequest{SQL: "VACUUM (FULL) " + q + ".plain"}, nil},
	}
	for _, c := range cases {
		got, err := b.Bind(ctx, c.req)
		if err != nil {
			t.Fatalf("%s: %v", c.req.SQL, err)
		}
		var ids []int64
		for _, g := range got {
			ids = append(ids, g.FactID)
		}
		if !reflect.DeepEqual(ids, c.want) {
			t.Errorf("%s: bound %v (%+v), want %v", c.req.SQL, ids, got, c.want)
		}
	}
	got, err := b.Bind(ctx, policy.ActionRequest{SQL: "VACUUM (FULL) " + q + ".legacy"})
	if err != nil || len(got) != 1 || got[0].FactID != legacy.ID {
		t.Fatalf("the inheritance parent itself stays bound: %+v %v", got, err)
	}
}
