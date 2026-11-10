package collector

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/snapstore"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// collection-03: another session's temporary tables and indexes (and
// pg_sage's own) are not application objects. No other session can
// vacuum, index or analyze them, and they vanish with their session, so
// the catalog reads leave every relpersistence 't' relation out. Unlogged
// tables ('u') are application tables and stay.
//
// No invalid-input or concurrency tests: notTempRel takes no input, and
// each read is one statement over a catalog snapshot.

// tempPermanentFixtureSQL is the application: a permanent public.stage
// with the same names the temporary session uses, an unlogged table and
// an inheritance parent with one permanent child.
const tempPermanentFixtureSQL = `
CREATE TABLE public.stage_parent (id int PRIMARY KEY);
CREATE TABLE public.stage (id int PRIMARY KEY, v int,
  parent_id int CONSTRAINT stage_perm_fk REFERENCES public.stage_parent (id))
  WITH (autovacuum_enabled = off);
CREATE INDEX stage_v ON public.stage (v);
INSERT INTO public.stage_parent VALUES (1);
INSERT INTO public.stage SELECT g, g % 10, 1 FROM generate_series(1, 1000) g;
DELETE FROM public.stage WHERE id > 900;
CREATE UNLOGGED TABLE public.stage_unlogged (id int PRIMARY KEY)
  WITH (autovacuum_enabled = off);
INSERT INTO public.stage_unlogged SELECT generate_series(1, 50);
CREATE TABLE public.stage_inh (id int);
CREATE TABLE public.stage_inh_perm () INHERITS (public.stage_inh);`

// tempSessionFixtureSQL is the other session's work: temporary tables of
// the same names with rows, dead tuples, an index, a foreign key, storage
// options and a temporary partitioned table.
const tempSessionFixtureSQL = `
CREATE TEMP TABLE stage_parent (id int PRIMARY KEY);
CREATE TEMP TABLE stage (id int PRIMARY KEY, v int,
  parent_id int CONSTRAINT stage_temp_fk REFERENCES stage_parent (id))
  WITH (fillfactor = 80);
CREATE INDEX stage_v ON stage (v);
INSERT INTO stage_parent VALUES (1);
INSERT INTO stage SELECT g, g % 10, 1 FROM generate_series(1, 10000) g;
DELETE FROM stage WHERE id > 9000;
CREATE TEMP TABLE stage_parted (k int) PARTITION BY RANGE (k);
CREATE TEMP TABLE stage_parted_p1 PARTITION OF stage_parted
  FOR VALUES FROM (0) TO (10);`

// tempChildFixtureSQL makes a temporary child of the permanent parent.
const tempChildFixtureSQL = `
CREATE TEMP TABLE stage_child () INHERITS (public.stage_inh);`

// tempFixture is a fresh bootstrapped database with the permanent fixture
// and a second session that holds its temporary objects open. It returns
// the collector, its pool and the temporary session.
func tempFixture(t *testing.T, permanent bool) (*Collector, *pgxpool.Pool, *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	dsn := testdb.CreateDatabase(t, "collector_temp")
	pool := bootstrappedPool(t, ctx, dsn)
	if permanent {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		if _, err := conn.Exec(ctx, tempPermanentFixtureSQL); err != nil {
			t.Fatalf("permanent fixture: %v", err)
		}
		flushStats(t, ctx, conn.Conn())
		conn.Release()
	} else if _, err := pool.Exec(ctx, "DROP EXTENSION IF EXISTS pg_hint_plan"); err != nil {
		t.Fatalf("drop the template's extension tables: %v", err) // hint_plan.hints
	}
	sql := tempSessionFixtureSQL
	if permanent {
		sql += tempChildFixtureSQL
	}
	temp := tempSession(t, ctx, dsn, sql)
	return New(pool, testConfig(), serverVersion(t, pool), noopLog), pool, temp
}

// tempSession opens a second session on dsn, runs sql there, flushes its
// statistics and keeps the session open until the test ends.
func tempSession(t *testing.T, ctx context.Context, dsn, sql string) *pgx.Conn {
	t.Helper()
	temp, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("temporary session: %v", err)
	}
	t.Cleanup(func() { _ = temp.Close(context.Background()) })
	if _, err := temp.Exec(ctx, sql); err != nil {
		t.Fatalf("temporary fixture: %v", err)
	}
	flushStats(t, ctx, temp)
	return temp
}

// tempRelations counts the temporary relations whose name starts with
// prefix, so a test can prove its temporary objects exist (or are gone).
func tempRelations(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	prefix string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_class
		WHERE relpersistence = 't' AND relname LIKE $1 || '%'`, prefix).Scan(&n); err != nil {
		t.Fatalf("count temporary relations: %v", err)
	}
	return n
}

func isTempSchema(schema string) bool {
	return strings.HasPrefix(schema, "pg_temp") || strings.HasPrefix(schema, "pg_toast_temp")
}

// assertNoTempObjects fails for every temporary object in snap's tables,
// indexes, foreign keys, partitions and configuration reloptions.
func assertNoTempObjects(t *testing.T, snap *Snapshot) {
	t.Helper()
	for _, ts := range snap.Tables {
		if isTempSchema(ts.SchemaName) || ts.Relpersistence == "t" {
			t.Errorf("temporary table collected: %s.%s (%s)", ts.SchemaName, ts.RelName,
				ts.Relpersistence)
		}
	}
	for _, ix := range snap.Indexes {
		if isTempSchema(ix.SchemaName) {
			t.Errorf("temporary index collected: %s.%s", ix.SchemaName, ix.IndexRelName)
		}
	}
	for _, fk := range snap.ForeignKeys {
		if fk.ConstraintName == "stage_temp_fk" {
			t.Errorf("temporary foreign key collected: %+v", fk)
		}
	}
	for _, p := range snap.Partitions {
		if isTempSchema(p.ChildSchema) || isTempSchema(p.ParentSchema) {
			t.Errorf("temporary inheritance collected: %+v", p)
		}
	}
	if snap.ConfigData == nil {
		t.Fatal("precondition: no configuration snapshot (advisor enabled)")
	}
	for _, ro := range snap.ConfigData.TableReloptions {
		if isTempSchema(ro.SchemaName) || strings.Contains(ro.Reloptions, "fillfactor=80") {
			t.Errorf("temporary table reloptions collected: %+v", ro)
		}
	}
}

func TestCollect_OtherSessionTempObjectsLeftOut(t *testing.T) {
	c, pool, _ := tempFixture(t, true)
	ctx := context.Background()
	// stage, stage_parent, stage_v, both pkeys, stage_child, stage_parted(_p1)
	if n := tempRelations(t, ctx, pool, "stage"); n < 8 {
		t.Fatalf("precondition: %d temporary stage relations, want >= 8", n)
	}
	snap, err := c.collect(ctx)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, cat := range []string{"tables", "indexes", "foreign_keys", "partitions",
		"config_data"} {
		if !snap.Available(cat) {
			t.Fatalf("%s unavailable: %s", cat, snap.Unavailable[cat])
		}
	}
	assertNoTempObjects(t, snap)
}

// tablesByName indexes snap's tables by schema-qualified name.
func tablesByName(snap *Snapshot) map[string]TableStats {
	out := map[string]TableStats{}
	for _, ts := range snap.Tables {
		out[ts.SchemaName+"."+ts.RelName] = ts
	}
	return out
}

func indexNames(snap *Snapshot) map[string]bool {
	out := map[string]bool{}
	for _, ix := range snap.Indexes {
		out[ix.SchemaName+"."+ix.IndexRelName] = true
	}
	return out
}

func TestCollect_PermanentSameNameTableKeptBesideTempOne(t *testing.T) {
	c, _, _ := tempFixture(t, true)
	snap, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	stage, ok := tablesByName(snap)["public.stage"]
	if !ok {
		t.Fatalf("public.stage missing: %v", tablesByName(snap))
	}
	if stage.NTupIns != 1000 || stage.NTupDel != 100 || stage.NLiveTup != 900 ||
		stage.NDeadTup != 100 || stage.Relpersistence != "p" {
		t.Errorf("public.stage counters = ins %d del %d live %d dead %d (%q), "+
			"want 1000/100/900/100 (p)", stage.NTupIns, stage.NTupDel, stage.NLiveTup,
			stage.NDeadTup, stage.Relpersistence)
	}
	names := indexNames(snap)
	for _, want := range []string{"public.stage_v", "public.stage_pkey",
		"public.stage_parent_pkey"} {
		if !names[want] {
			t.Errorf("index %s missing: %v", want, names)
		}
	}
	assertPermanentCatalog(t, snap)
}

// assertPermanentCatalog checks the permanent foreign key, inheritance
// child and reloptions survive beside their temporary namesakes.
func assertPermanentCatalog(t *testing.T, snap *Snapshot) {
	t.Helper()
	var fks []string
	for _, fk := range snap.ForeignKeys {
		fks = append(fks, fk.ConstraintName)
	}
	if strings.Join(fks, ",") != "stage_perm_fk" {
		t.Errorf("foreign keys = %v, want only stage_perm_fk", fks)
	}
	if len(snap.Partitions) != 1 || snap.Partitions[0] != (PartitionInfo{
		ChildTable: "stage_inh_perm", ChildSchema: "public",
		ParentTable: "stage_inh", ParentSchema: "public"}) {
		t.Errorf("partitions = %+v, want only public.stage_inh_perm", snap.Partitions)
	}
	if snap.ConfigData == nil {
		t.Fatal("precondition: no configuration snapshot (advisor enabled)")
	}
	var relopts []string
	for _, ro := range snap.ConfigData.TableReloptions {
		relopts = append(relopts, ro.SchemaName+"."+ro.RelName+"="+ro.Reloptions)
	}
	sort.Strings(relopts) // the read has no order
	want := "public.stage={autovacuum_enabled=off}," +
		"public.stage_unlogged={autovacuum_enabled=off}"
	if got := strings.Join(relopts, ","); got != want {
		t.Errorf("configuration reloptions = %s, want %s", got, want)
	}
}

func TestCollect_UnloggedTableAndIndexStillCollected(t *testing.T) {
	c, _, _ := tempFixture(t, true)
	snap, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	un, ok := tablesByName(snap)["public.stage_unlogged"]
	if !ok {
		t.Fatalf("unlogged table missing: %v", tablesByName(snap))
	}
	if un.Relpersistence != "u" || !un.IsUnlogged() || un.NTupIns != 50 {
		t.Errorf("public.stage_unlogged = %q unlogged %v ins %d, want u/true/50",
			un.Relpersistence, un.IsUnlogged(), un.NTupIns)
	}
	if !indexNames(snap)["public.stage_unlogged_pkey"] {
		t.Errorf("unlogged table's index missing: %v", indexNames(snap))
	}
}

// A database whose only user tables are temporary has an empty tables
// category that is still available: known to be empty, not unknown.
func TestCollect_OnlyTempTablesGiveEmptyAvailableTables(t *testing.T) {
	c, pool, _ := tempFixture(t, false)
	ctx := context.Background()
	if n := tempRelations(t, ctx, pool, "stage"); n < 7 {
		t.Fatalf("precondition: %d temporary stage relations, want >= 7", n)
	}
	snap, err := c.collect(ctx)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !snap.Available("tables") || !snap.Available("indexes") {
		t.Fatalf("tables/indexes unavailable: %v", snap.Unavailable)
	}
	if len(snap.Tables) != 0 || len(snap.Indexes) != 0 || len(snap.ForeignKeys) != 0 {
		t.Fatalf("collected %d tables, %d indexes, %d foreign keys, want none: %+v",
			len(snap.Tables), len(snap.Indexes), len(snap.ForeignKeys), snap.Tables)
	}
	rows, err := snapshotRows(snap)
	if err != nil {
		t.Fatalf("snapshotRows: %v", err)
	}
	for _, r := range rows {
		if r.Category == "tables" {
			return
		}
	}
	t.Fatal("no tables row persisted for an available, empty category")
}

// State transition: the temporary session ends between two cycles. The
// temporary objects were never stored, so the second cycle's tables and
// indexes deltas delete nothing.
func TestPersist_EndedTempSessionLeavesNoDeltaDeletes(t *testing.T) {
	c, pool, temp := tempFixture(t, true)
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Second)
	collectAndPersist(t, ctx, c, start)
	if err := temp.Close(ctx); err != nil {
		t.Fatalf("end temporary session: %v", err)
	}
	waitNoTempRelations(t, ctx, pool, "stage")
	collectAndPersist(t, ctx, c, start.Add(time.Minute))
	for _, cat := range []string{"tables", "indexes"} {
		var firstID int64
		var secondBase *int64
		var deletes, firstDoc string
		if err := pool.QueryRow(ctx, `SELECT min(id),
			(SELECT base_id FROM sage.snapshots WHERE category = $1
			  ORDER BY collected_at DESC LIMIT 1),
			COALESCE((SELECT data->>'d' FROM sage.snapshots WHERE category = $1
			  ORDER BY collected_at DESC LIMIT 1), ''),
			(SELECT `+snapstore.DataSQL("")+`::text FROM sage.snapshots
			  WHERE category = $1 ORDER BY collected_at LIMIT 1)
			FROM sage.snapshots WHERE category = $1`, cat).
			Scan(&firstID, &secondBase, &deletes, &firstDoc); err != nil {
			t.Fatalf("%s rows: %v", cat, err)
		}
		if strings.Contains(firstDoc, "pg_temp") {
			t.Errorf("%s: first cycle stored temporary objects: %s", cat, firstDoc)
		}
		if secondBase == nil || *secondBase != firstID {
			t.Errorf("%s: second row base %v, want a delta on keyframe %d", cat,
				secondBase, firstID)
		}
		if deletes != "" {
			t.Errorf("%s: second cycle's delta deletes elements %s", cat, deletes)
		}
	}
}

func collectAndPersist(t *testing.T, ctx context.Context, c *Collector, at time.Time) {
	t.Helper()
	snap, err := c.collect(ctx)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	snap.CollectedAt = at
	if err := c.persist(ctx, snap); err != nil {
		t.Fatalf("persist at %s: %v", at, err)
	}
}

// waitNoTempRelations waits until the ended session's backend has dropped
// its temporary relations (it does so as it exits, after the client left).
func waitNoTempRelations(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	prefix string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for tempRelations(t, ctx, pool, prefix) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("temporary relations still present 15 s after the session ended")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
