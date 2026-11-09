package firstlook

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// No concurrent access tests here: the reads keep no state and run in the
// caller's transaction; the rules are pure functions over their result.

// partitionedSchema creates schema s with table p range-partitioned on k
// into p1, p2 and p3, each holding rows, and runs the extra statements.
func partitionedSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	prefix string, extra ...string) (string, string) {
	t.Helper()
	s := uniqueSchema(prefix)
	q := pgx.Identifier{s}.Sanitize()
	stmts := []string{"CREATE SCHEMA " + q,
		"CREATE TABLE " + q + ".p (id int, k int) PARTITION BY RANGE (k)",
		"CREATE TABLE " + q + ".p1 PARTITION OF " + q + ".p FOR VALUES FROM (0) TO (100)",
		"CREATE TABLE " + q + ".p2 PARTITION OF " + q + ".p FOR VALUES FROM (100) TO (200)",
		"CREATE TABLE " + q + ".p3 PARTITION OF " + q + ".p FOR VALUES FROM (200) TO (300)",
		"INSERT INTO " + q + ".p SELECT g, g % 300 FROM generate_series(1, 3000) g",
	}
	for _, e := range extra {
		stmts = append(stmts, strings.ReplaceAll(e, "$S", q))
	}
	execAll(t, ctx, pool, append(stmts, "ANALYZE "+q+".p")...)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+q+" CASCADE")
	})
	return s, q
}

// readSchema reads the indexes (and the unattached partitions of the
// invalid partitioned ones) and keeps those in schema s. It ends its
// snapshot before returning: a concurrent index build waits for it.
func readSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s string) []Index {
	t.Helper()
	tx := snapshotTx(t, ctx, pool)
	defer func() { _ = tx.Rollback(ctx) }()
	all, _, err := readIndexes(ctx, tx)
	if err != nil {
		t.Fatalf("read indexes: %v", err)
	}
	if err := readUnattached(ctx, tx, all); err != nil {
		t.Fatalf("read unattached partitions: %v", err)
	}
	var out []Index
	for _, x := range all {
		if x.Schema == s {
			out = append(out, x)
		}
	}
	return out
}

func TestPartitionReadMarksParentsAndChildren(t *testing.T) {
	pool, ctx := livePool(t)
	s, _ := partitionedSchema(t, ctx, pool, "part_read_",
		"CREATE INDEX p_k ON $S.p (k)",
		"CREATE TABLE $S.q (id int, k int) PARTITION BY RANGE (k)",
		"CREATE TABLE $S.q1 PARTITION OF $S.q FOR VALUES FROM (0) TO (10) PARTITION BY RANGE (id)",
		"CREATE TABLE $S.q1a PARTITION OF $S.q1 FOR VALUES FROM (0) TO (10)",
		"CREATE INDEX q_k ON $S.q (k)",
		"CREATE TABLE $S.d PARTITION OF $S.p FOR VALUES FROM (300) TO (400)",
		"ALTER TABLE $S.p DETACH PARTITION $S.d",
		"CREATE TABLE $S.plain (a int)", "CREATE INDEX plain_a ON $S.plain (a)")
	idx := readSchema(t, ctx, pool, s)
	parent := byName(t, idx, s, "p_k")
	if !parent.Partitioned || parent.IsPartition || parent.ParentOID != 0 {
		t.Fatalf("p_k read as %+v, want a top-level partitioned index", parent)
	}
	for _, name := range []string{"p1_k_idx", "p2_k_idx", "p3_k_idx"} {
		c := byName(t, idx, s, name)
		if c.Partitioned || !c.IsPartition || c.ParentOID != parent.OID {
			t.Fatalf("%s read as %+v, want a leaf attached to p_k (%d)", name, c, parent.OID)
		}
	}
	mid, leaf := byName(t, idx, s, "q1_k_idx"), byName(t, idx, s, "q1a_k_idx")
	if !mid.Partitioned || !mid.IsPartition || mid.ParentOID != byName(t, idx, s, "q_k").OID {
		t.Fatalf("q1_k_idx read as %+v, want a middle-level partitioned index", mid)
	}
	if leaf.Partitioned || !leaf.IsPartition || leaf.ParentOID != mid.OID {
		t.Fatalf("q1a_k_idx read as %+v, want a leaf of q1_k_idx", leaf)
	}
	for _, name := range []string{"d_k_idx", "plain_a"} {
		if x := byName(t, idx, s, name); x.Partitioned || x.IsPartition || x.ParentOID != 0 {
			t.Fatalf("%s read as %+v, want an ordinary index", name, x)
		}
	}
}

func TestPartitionNeverScannedLive(t *testing.T) {
	pool, ctx := livePool(t)
	s, q := partitionedSchema(t, ctx, pool, "part_scan_", "CREATE INDEX p_k ON $S.p (k)")
	now := time.Now()
	idle := NeverScannedIndexes(readSchema(t, ctx, pool, s), nil, week, now)
	if len(idle) != 1 || idle[0].Object != s+".p_k" {
		t.Fatalf("no partition scanned: never-scanned %v, want exactly %s.p_k", rulesOf(idle), s)
	}
	noConcurrently(t, idle[0])
	scanOnce(t, ctx, pool, "SELECT count(*) FROM "+q+".p WHERE k = 150")
	if got := NeverScannedIndexes(readSchema(t, ctx, pool, s), nil, week,
		now); len(got) != 0 {
		t.Fatalf("p2 scanned: never-scanned %v, want none", rulesOf(got))
	}
}

// A partitioned table with no partitions: its index has no scan sum.
func TestPartitionParentWithoutPartitionsLive(t *testing.T) {
	pool, ctx := livePool(t)
	s := uniqueSchema("part_empty_")
	q := pgx.Identifier{s}.Sanitize()
	execAll(t, ctx, pool, "CREATE SCHEMA "+q,
		"CREATE TABLE "+q+".e (id int) PARTITION BY RANGE (id)",
		"CREATE INDEX e_id ON "+q+".e (id)")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+q+" CASCADE")
	})
	idx := readSchema(t, ctx, pool, s)
	if x := byName(t, idx, s, "e_id"); !x.Partitioned {
		t.Fatalf("e_id read as %+v, want partitioned", x)
	}
	if got := NeverScannedIndexes(idx, nil, week, time.Now()); len(got) != 0 {
		t.Fatalf("parent only: never-scanned %v, want none", rulesOf(got))
	}
}

// runRolledBack runs sql in a transaction it rolls back: the statement is
// accepted by PostgreSQL inside a transaction block.
func runRolledBack(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, sql); err != nil {
		t.Fatalf("suggested SQL %q failed: %v", sql, err)
	}
}

func TestPartitionDuplicateLive(t *testing.T) {
	pool, ctx := livePool(t)
	s, _ := partitionedSchema(t, ctx, pool, "part_dup_",
		"CREATE INDEX p_k ON $S.p (k)", "CREATE INDEX p_k_copy ON $S.p (k)")
	items, flagged := DuplicateIndexes(readSchema(t, ctx, pool, s))
	if len(items) != 1 || items[0].Object != s+".p_k_copy" || len(flagged) != 1 {
		t.Fatalf("duplicates %v, want one for %s.p_k_copy", rulesOf(items), s)
	}
	noConcurrently(t, items[0])
	runRolledBack(t, ctx, pool, items[0].SuggestedSQL)
}

func TestPartitionRedundantLive(t *testing.T) {
	pool, ctx := livePool(t)
	s, _ := partitionedSchema(t, ctx, pool, "part_red_",
		"CREATE INDEX p_k ON $S.p (k)", "CREATE INDEX p_k_id ON $S.p (k, id)")
	items, _ := DuplicateIndexes(readSchema(t, ctx, pool, s))
	if len(items) != 1 || items[0].Rule != RuleRedundantIndex || items[0].Object != s+".p_k" {
		t.Fatalf("redundant %v, want one for %s.p_k", rulesOf(items), s)
	}
	noConcurrently(t, items[0])
	runRolledBack(t, ctx, pool, items[0].SuggestedSQL)
}

// An index created ON ONLY the parent with p1 attached: the item names p2
// and p3, and its SQL, run statement by statement, makes the index valid.
func TestPartitionOnOnlyInvalidLive(t *testing.T) {
	pool, ctx := livePool(t)
	s, _ := partitionedSchema(t, ctx, pool, "part_only_",
		"CREATE INDEX p_only ON ONLY $S.p (id)", "CREATE INDEX p1_id ON $S.p1 (id)",
		"ALTER INDEX $S.p_only ATTACH PARTITION $S.p1_id")
	items := InvalidIndexes(readSchema(t, ctx, pool, s), IndexBuilds{})
	if len(items) != 1 || items[0].Object != s+".p_only" {
		t.Fatalf("invalid %v, want one for %s.p_only", rulesOf(items), s)
	}
	it := items[0]
	if strings.Contains(it.SuggestedSQL+it.Recommendation, "REINDEX") ||
		!strings.Contains(it.SuggestedSQL, "ATTACH PARTITION") ||
		!strings.Contains(it.Detail, s+".p2") || !strings.Contains(it.Detail, s+".p3") ||
		strings.Contains(it.Detail, s+".p1,") {
		t.Fatalf("ON ONLY item %+v, want p2 and p3 named and ATTACH PARTITION", it)
	}
	for _, stmt := range strings.Split(it.SuggestedSQL, "\n") {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("suggested %q: %v", stmt, err)
		}
	}
	var valid bool
	err := pool.QueryRow(ctx, "SELECT i.indisvalid FROM pg_index i JOIN pg_class c "+
		"ON c.oid = i.indexrelid JOIN pg_namespace n ON n.oid = c.relnamespace "+
		"WHERE n.nspname = $1 AND c.relname = 'p_only'", s).Scan(&valid)
	if err != nil || !valid {
		t.Fatalf("after the suggested SQL p_only valid=%t err=%v, want valid", valid, err)
	}
}

// Failures name the step: an aborted transaction.
func TestPartitionUnattachedReadErrors(t *testing.T) {
	pool, ctx := livePool(t)
	tx := snapshotTx(t, ctx, pool)
	idx := []Index{{OID: 1, Partitioned: true}}
	if _, err := tx.Exec(ctx, "SELECT 1/0"); err == nil {
		t.Fatal("division by zero succeeded")
	}
	err := readUnattached(ctx, tx, idx)
	if err == nil || !strings.Contains(err.Error(), "read unattached partitions") {
		t.Fatalf("aborted transaction: %v, want a read unattached partitions error", err)
	}
	if idx[0].UnattachedRead {
		t.Fatal("a failed read marked the index as read")
	}
	// Nothing to read: no statement is sent, even on an aborted transaction.
	if err := readUnattached(ctx, tx, []Index{{OID: 2}}); err != nil {
		t.Fatalf("no invalid partitioned index: %v, want nil", err)
	}
}

func TestPartitionUnindexedForeignKeyLive(t *testing.T) {
	pool, ctx := livePool(t)
	s, _ := partitionedSchema(t, ctx, pool, "part_fk_",
		"CREATE TABLE $S.ref (id int PRIMARY KEY)",
		"INSERT INTO $S.ref SELECT g FROM generate_series(1, 3000) g",
		"ALTER TABLE $S.p ADD CONSTRAINT p_id_fk FOREIGN KEY (id) REFERENCES $S.ref (id)")
	tx := snapshotTx(t, ctx, pool)
	fks, err := readForeignKeys(ctx, tx)
	if err != nil {
		t.Fatalf("read foreign keys: %v", err)
	}
	var mine []ForeignKey
	for _, fk := range fks {
		if fk.Schema == s {
			mine = append(mine, fk)
		}
	}
	items := UnindexedForeignKeys(mine, readSchema(t, ctx, pool, s), 0)
	if len(items) != 1 || !mine[0].Partitioned {
		t.Fatalf("foreign keys %+v gave %v, want one partitioned item", mine, rulesOf(items))
	}
	noConcurrently(t, Item{Rule: items[0].Rule, SuggestedSQL: items[0].SuggestedSQL})
	runRolledBack(t, ctx, pool, items[0].SuggestedSQL)
}

// The whole index pass on a partitioned schema: the invalid ON ONLY index
// names its partition, the duplicate parent is reported once, and no
// attached partition index has an item of its own.
func TestPartitionIndexChecksLive(t *testing.T) {
	pool, ctx := livePool(t)
	s, _ := partitionedSchema(t, ctx, pool, "part_pass_",
		"CREATE INDEX p_k ON $S.p (k)", "CREATE INDEX p_k_copy ON $S.p (k)",
		"CREATE INDEX p_only ON ONLY $S.p (id)")
	p := capPass(50)
	tx := snapshotTx(t, ctx, pool)
	if _, err := p.indexChecks(ctx, tx); err != nil {
		t.Fatalf("index checks: %v", err)
	}
	inv := reportItem(t, p.report, RuleInvalidIndex, s+".p_only")
	if strings.Count(inv.SuggestedSQL, "ATTACH PARTITION") != 3 ||
		strings.Contains(inv.SuggestedSQL, "REINDEX") {
		t.Fatalf("p_only SQL %q, want three ATTACH PARTITION and no REINDEX", inv.SuggestedSQL)
	}
	reportItem(t, p.report, RuleDuplicateIndex, s+".p_k_copy")
	var mine []string
	for _, it := range p.report.Items {
		if strings.HasPrefix(it.Object, s+".") {
			mine = append(mine, it.Rule+":"+it.Object)
		}
	}
	// p_k is never scanned, p_k_copy is the duplicate, p_only is invalid.
	want := map[string]bool{RuleInvalidIndex + ":" + s + ".p_only": true,
		RuleDuplicateIndex + ":" + s + ".p_k_copy": true,
		RuleNeverScannedIndex + ":" + s + ".p_k":   true}
	if len(mine) != len(want) {
		t.Fatalf("items %v, want exactly %v", mine, want)
	}
	for _, m := range mine {
		if !want[m] {
			t.Fatalf("unexpected item %s among %v", m, mine)
		}
	}
}
