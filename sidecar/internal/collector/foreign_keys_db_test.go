package collector

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// collection-04: foreign keys are read one row per constraint, with the
// schema of both sides, the referencing columns in conkey order, and no
// partition clones (conparentid <> 0). Two tenants with the same table
// and constraint names are two distinct constraints, so the stored
// document has a unique identity and is delta encoded.
//
// No concurrency test: the read is one statement in the collector
// goroutine over a catalog snapshot.

const twoTenantFKFixture = `
CREATE SCHEMA tenant_a;
CREATE SCHEMA tenant_b;
CREATE TABLE tenant_a.customers (id int PRIMARY KEY);
CREATE TABLE tenant_b.customers (id int PRIMARY KEY);
CREATE TABLE tenant_a.orders (id int PRIMARY KEY, customer_id int,
  CONSTRAINT orders_customer_fk FOREIGN KEY (customer_id)
    REFERENCES tenant_a.customers (id));
CREATE TABLE tenant_b.orders (id int PRIMARY KEY, customer_id int,
  CONSTRAINT orders_customer_fk FOREIGN KEY (customer_id)
    REFERENCES tenant_b.customers (id));`

// fkPool is a fresh bootstrapped database holding fixture.
func fkPool(t *testing.T, ctx context.Context, label, fixture string) *pgxpool.Pool {
	t.Helper()
	pool := bootstrappedPool(t, ctx, testdb.CreateDatabase(t, label))
	if fixture != "" {
		if _, err := pool.Exec(ctx, fixture); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}
	return pool
}

// collectFKs runs one collection and returns its foreign keys, failing
// when the category is unavailable.
func collectFKs(t *testing.T, ctx context.Context, c *Collector) (*Snapshot, []ForeignKey) {
	t.Helper()
	snap, err := c.collect(ctx)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !snap.Available("foreign_keys") {
		t.Fatalf("foreign_keys unavailable: %s", snap.Unavailable["foreign_keys"])
	}
	return snap, snap.ForeignKeys
}

// fksNamed returns the collected constraints called name.
func fksNamed(fks []ForeignKey, name string) []ForeignKey {
	var out []ForeignKey
	for _, fk := range fks {
		if fk.ConstraintName == name {
			out = append(out, fk)
		}
	}
	return out
}

func TestForeignKeys_TwoTenantsAreDistinctAndDeltaEncoded(t *testing.T) {
	ctx := context.Background()
	pool := fkPool(t, ctx, "fk_tenants", twoTenantFKFixture)
	c := New(pool, testConfig(), 170000, noopLog)
	snap, fks := collectFKs(t, ctx, c)
	got := fksNamed(fks, "orders_customer_fk")
	if len(got) != 2 {
		t.Fatalf("orders_customer_fk rows = %d (%+v), want 2", len(got), got)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].Schema < got[j].Schema })
	for i, schema := range []string{"tenant_a", "tenant_b"} {
		fk := got[i]
		if fk.Schema != schema || fk.TableName != "orders" || fk.RefSchema != schema ||
			fk.ReferencedTable != "customers" ||
			!reflect.DeepEqual(fk.Columns, []string{"customer_id"}) || fk.OID == 0 {
			t.Errorf("constraint %d = %+v, want %s.orders(customer_id) -> %s.customers",
				i, fk, schema, schema)
		}
	}
	if got[0].OID == got[1].OID {
		t.Errorf("both tenants report oid %d, want distinct constraints", got[0].OID)
	}
	start := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 2; i++ {
		snap.CollectedAt = start.Add(time.Duration(i) * time.Minute)
		if err := c.persist(ctx, snap); err != nil {
			t.Fatalf("persist %d: %v", i, err)
		}
	}
	var first int64
	var secondBase *int64
	if err := pool.QueryRow(ctx, `SELECT min(id),
		(SELECT base_id FROM sage.snapshots WHERE category = 'foreign_keys'
		  ORDER BY collected_at DESC LIMIT 1)
		FROM sage.snapshots WHERE category = 'foreign_keys'`).
		Scan(&first, &secondBase); err != nil {
		t.Fatalf("foreign_keys rows: %v", err)
	}
	if secondBase == nil || *secondBase != first {
		t.Fatalf("second foreign_keys row base %v (first id %d), want a delta on it",
			secondBase, first)
	}
	assertStoredAsCollected(t, ctx, pool, []*Snapshot{snap})
}

// Happy path: the columns of a composite key keep their declared order,
// not attnum order: (b, a) stays (b, a).
func TestForeignKeys_CompositeKeepsConkeyOrder(t *testing.T) {
	ctx := context.Background()
	pool := fkPool(t, ctx, "fk_composite", `
CREATE TABLE public.parent (x int, y int, PRIMARY KEY (x, y));
CREATE TABLE public.child (id int PRIMARY KEY, a int, b int,
  CONSTRAINT child_parent_fk FOREIGN KEY (b, a) REFERENCES public.parent (x, y));`)
	_, fks := collectFKs(t, ctx, New(pool, testConfig(), 170000, noopLog))
	got := fksNamed(fks, "child_parent_fk")
	if len(got) != 1 {
		t.Fatalf("child_parent_fk rows = %d (%+v), want 1 row per constraint",
			len(got), got)
	}
	if want := []string{"b", "a"}; !reflect.DeepEqual(got[0].Columns, want) {
		t.Fatalf("columns = %v, want %v (conkey order)", got[0].Columns, want)
	}
	if got[0].Schema != "public" || got[0].RefSchema != "public" {
		t.Fatalf("schemas = %q -> %q, want public -> public", got[0].Schema,
			got[0].RefSchema)
	}
}

// partitionFKFixture: events is partitioned 30 ways and references
// customers (30 clones on the referencing side); refs references the
// partitioned accounts (one clone per referenced partition).
const partitionFKFixture = `
CREATE TABLE public.customers (id int PRIMARY KEY);
CREATE TABLE public.events (id int, customer_id int, day int,
  CONSTRAINT events_customer_fk FOREIGN KEY (customer_id)
    REFERENCES public.customers (id)) PARTITION BY RANGE (day);
DO $$ BEGIN
  FOR p IN 0..29 LOOP
    EXECUTE format('CREATE TABLE public.events_p%s PARTITION OF public.events
      FOR VALUES FROM (%s) TO (%s)', p, p, p + 1);
  END LOOP;
END $$;
CREATE TABLE public.accounts (id int PRIMARY KEY) PARTITION BY RANGE (id);
CREATE TABLE public.accounts_p0 PARTITION OF public.accounts FOR VALUES FROM (0) TO (100);
CREATE TABLE public.accounts_p1 PARTITION OF public.accounts FOR VALUES FROM (100) TO (200);
CREATE TABLE public.accounts_p2 PARTITION OF public.accounts FOR VALUES FROM (200) TO (300);
CREATE TABLE public.refs (id int PRIMARY KEY, account_id int,
  CONSTRAINT refs_account_fk FOREIGN KEY (account_id) REFERENCES public.accounts (id));`

// Boundary: partition clones are left out on both sides, so a key on a
// table with 30 partitions, and a key referencing a partitioned table,
// are one row each, naming the partitioned parent.
func TestForeignKeys_PartitionClonesLeftOut(t *testing.T) {
	ctx := context.Background()
	pool := fkPool(t, ctx, "fk_partitions", partitionFKFixture)
	var clones int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
		WHERE contype = 'f' AND conparentid <> 0`).Scan(&clones); err != nil || clones < 33 {
		t.Fatalf("precondition: %d partition clones (%v), want >= 33", clones, err)
	}
	_, fks := collectFKs(t, ctx, New(pool, testConfig(), 170000, noopLog))
	if len(fks) != 2 {
		t.Fatalf("foreign keys = %d (%+v), want 2 (no partition clones)", len(fks), fks)
	}
	ev := fksNamed(fks, "events_customer_fk")
	if len(ev) != 1 || ev[0].TableName != "events" || ev[0].ReferencedTable != "customers" {
		t.Errorf("events_customer_fk = %+v, want one row on the partitioned events", ev)
	}
	rf := fksNamed(fks, "refs_account_fk")
	if len(rf) != 1 || rf[0].TableName != "refs" || rf[0].ReferencedTable != "accounts" {
		t.Errorf("refs_account_fk = %+v, want one row referencing accounts", rf)
	}
}

// Nil/empty: a database without foreign keys has an available, empty
// category, stored as an empty list (a real "none", delta encodable),
// not null.
func TestForeignKeys_NoneIsAvailableAndEmpty(t *testing.T) {
	ctx := context.Background()
	pool := fkPool(t, ctx, "fk_none", `CREATE TABLE public.lonely (id int PRIMARY KEY)`)
	snap, fks := collectFKs(t, ctx, New(pool, testConfig(), 170000, noopLog))
	if len(fks) != 0 {
		t.Fatalf("foreign keys = %+v, want none", fks)
	}
	rows, err := snapshotRows(snap)
	if err != nil {
		t.Fatalf("snapshotRows: %v", err)
	}
	for _, r := range rows {
		if r.Category == "foreign_keys" {
			if string(r.Data) != "[]" {
				t.Fatalf("stored foreign_keys = %s, want []", r.Data)
			}
			return
		}
	}
	t.Fatal("no foreign_keys document written")
}
