package collector

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/snapstore"
)

// rules-03: the collector reports which indexes are attached to a
// partitioned index (pg_inherits) and reads the partitioned (relkind I)
// indexes themselves, which pg_stat_user_indexes leaves out.
//
// No concurrent access tests: these reads run on the collector's single
// loop goroutine.

const piSchema = "rules03_pi"

// createPartitionedIndexFixture builds rules03_pi.p partitioned by a into
// p_1, p_2 and p_3 (itself partitioned by b into p_3a), with partitioned
// indexes of each shape, a standalone index on p_1, and a plain table.
func createPartitionedIndexFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+piSchema+" CASCADE")
	})
	for _, sql := range []string{
		"DROP SCHEMA IF EXISTS " + piSchema + " CASCADE",
		"CREATE SCHEMA " + piSchema,
		"SET search_path = " + piSchema,
		"CREATE TABLE p (a int, b int, c int, t text) PARTITION BY RANGE (a)",
		"CREATE TABLE p_1 PARTITION OF p FOR VALUES FROM (0) TO (10)",
		"CREATE TABLE p_2 PARTITION OF p FOR VALUES FROM (10) TO (20)",
		"CREATE TABLE p_3 PARTITION OF p FOR VALUES FROM (20) TO (30) PARTITION BY RANGE (b)",
		"CREATE TABLE p_3a PARTITION OF p_3 FOR VALUES FROM (0) TO (100)",
		"CREATE INDEX p_ab_idx ON p (a, b) INCLUDE (c)",
		"CREATE INDEX p_expr_idx ON p ((lower(t)))",
		"CREATE UNIQUE INDEX p_ab_uniq ON p (a, b)",
		"CREATE INDEX p_part ON p (b) WHERE c > 0",
		"CREATE INDEX p_only ON ONLY p (c)",
		"CREATE INDEX p_1_c_solo ON p_1 (c)",
		"CREATE TABLE plain_t (x int)",
		"CREATE INDEX plain_x ON plain_t (x)",
		"RESET search_path",
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}

func fixtureParents(t *testing.T, c *Collector) map[string]PartitionedIndex {
	t.Helper()
	got, err := c.collectPartitionedIndexes(context.Background())
	if err != nil {
		t.Fatalf("collectPartitionedIndexes: %v", err)
	}
	out := map[string]PartitionedIndex{}
	for _, pi := range got {
		if pi.SchemaName == piSchema {
			out[pi.IndexRelName] = pi
		}
	}
	return out
}

func fixtureLeaves(t *testing.T, c *Collector) map[string]IndexStats {
	t.Helper()
	got, err := c.collectIndexes(context.Background())
	if err != nil {
		t.Fatalf("collectIndexes: %v", err)
	}
	out := map[string]IndexStats{}
	for _, idx := range got {
		if idx.SchemaName == piSchema {
			out[idx.IndexRelName] = idx
		}
	}
	return out
}

// leafOf is the name of the index on table attached to parent.
func leafOf(leaves map[string]IndexStats, table string, parent uint32) string {
	for name, idx := range leaves {
		if idx.RelName == table && idx.ParentIndexOID == parent {
			return name
		}
	}
	return ""
}

func TestCollectPartitionedIndexes_ReadsParentsAndAttachment(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	createPartitionedIndexFixture(t, ctx, pool)
	cfg := testConfig()
	cfg.Collector.BatchSize = 2 // several pages
	c := New(pool, cfg, serverVersion(t, pool), noopLog)
	parents := fixtureParents(t, c)
	ab := assertFixtureParents(t, parents)
	leaves := fixtureLeaves(t, c)
	if leafOf(leaves, "p_1", ab.IndexRelID) == "" || leafOf(leaves, "p_2", ab.IndexRelID) == "" {
		t.Errorf("no leaf of p_ab_idx on p_1/p_2 among %v", leaves)
	}
	if leaves["p_1_c_solo"].ParentIndexOID != 0 || leaves["plain_x"].ParentIndexOID != 0 {
		t.Errorf("standalone %+v / plain %+v carry a parent index",
			leaves["p_1_c_solo"], leaves["plain_x"])
	}
	for name, idx := range leaves {
		if idx.RelName == "p_3a" && idx.ParentIndexOID == 0 {
			t.Errorf("p_3a leaf %s is not attached to p_3's index", name)
		}
	}
}

// assertFixtureParents checks the partitioned indexes read from the
// fixture and returns p_ab_idx.
func assertFixtureParents(t *testing.T, parents map[string]PartitionedIndex) PartitionedIndex {
	t.Helper()
	ab := parents["p_ab_idx"]
	if ab.IndexRelID == 0 || ab.RelName != "p" || ab.ParentIndexOID != 0 || !ab.IsValid ||
		ab.IsUnique || ab.IsPartial || !slices.Equal(ab.KeyColumns, []string{"a", "b"}) {
		t.Fatalf("p_ab_idx = %+v, want valid top-level keys [a b] (INCLUDE left out)", ab)
	}
	if got := parents["p_expr_idx"].KeyColumns; !slices.Equal(got, []string{""}) {
		t.Errorf("expression index keys = %q, want one empty (non-column) key", got)
	}
	if !parents["p_ab_uniq"].IsUnique || !parents["p_part"].IsPartial {
		t.Errorf("unique %+v / partial %+v flags lost", parents["p_ab_uniq"], parents["p_part"])
	}
	if parents["p_only"].IsValid {
		t.Error("ON ONLY index with unattached partitions reported valid")
	}
	// p_3's own indexes are partitioned indexes attached to the top ones.
	nested := 0
	for _, pi := range parents {
		if pi.RelName != "p_3" {
			continue
		}
		nested++
		if pi.ParentIndexOID == 0 {
			t.Errorf("intermediate %s has no parent index", pi.IndexRelName)
		}
	}
	if nested < 4 {
		t.Errorf("found %d intermediate partitioned indexes on p_3, want 4", nested)
	}
	for _, pi := range parents {
		if pi.RelName == "p_1" || pi.RelName == "plain_t" {
			t.Errorf("leaf or plain index %s read as partitioned", pi.IndexRelName)
		}
	}
	return ab
}

func TestCollectPartitionedIndexes_DetachAndAttach(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	createPartitionedIndexFixture(t, ctx, pool)
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	ab := fixtureParents(t, c)["p_ab_idx"].IndexRelID
	child := leafOf(fixtureLeaves(t, c), "p_2", ab)
	if ab == 0 || child == "" {
		t.Fatal("no p_ab_idx leaf on p_2")
	}
	exec := func(sql string) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec("ALTER TABLE " + piSchema + ".p DETACH PARTITION " + piSchema + ".p_2")
	if got := fixtureLeaves(t, c)[child].ParentIndexOID; got != 0 {
		t.Fatalf("detached partition's index parent = %d, want 0 (standalone)", got)
	}
	exec("ALTER TABLE " + piSchema + ".p ATTACH PARTITION " + piSchema +
		".p_2 FOR VALUES FROM (10) TO (20)")
	if got := fixtureLeaves(t, c)[child].ParentIndexOID; got != ab {
		t.Fatalf("reattached index parent = %d, want p_ab_idx %d", got, ab)
	}
	only := fixtureParents(t, c)["p_only"].IndexRelID
	exec("ALTER INDEX " + piSchema + ".p_only ATTACH PARTITION " + piSchema + ".p_1_c_solo")
	if got := fixtureLeaves(t, c)["p_1_c_solo"].ParentIndexOID; got != only {
		t.Fatalf("attached standalone parent = %d, want p_only %d", got, only)
	}
}

func TestCollectPartitionedIndexes_ErrorNamesCategory(t *testing.T) {
	pool := testPool(t)
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.collectPartitionedIndexes(ctx)
	if err == nil || !strings.Contains(err.Error(), "partitioned indexes") {
		t.Fatalf("err = %v, want one naming the partitioned indexes read", err)
	}
}

func TestCollect_PartitionCategoriesMarkedUnavailable(t *testing.T) {
	pool := testPool(t)
	for _, cat := range []string{"partitions", "partitioned_indexes"} {
		t.Run(cat, func(t *testing.T) {
			c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
			c.overrideStep(cat, func(context.Context, *Snapshot) error {
				return errors.New("canceling statement due to statement timeout")
			})
			snap, err := c.collect(context.Background())
			if err != nil {
				t.Fatalf("collect: %v", err)
			}
			if !strings.Contains(snap.Unavailable[cat], "statement timeout") {
				t.Fatalf("Unavailable = %v, want %s marked unknown", snap.Unavailable, cat)
			}
			rows, err := snapshotRows(snap)
			if err != nil {
				t.Fatalf("snapshotRows: %v", err)
			}
			for _, r := range rows {
				if r.Category == cat {
					t.Errorf("unavailable %s was persisted as empty", cat)
				}
			}
		})
	}
}

func TestPartitionedIndexes_PersistedChangeOnly(t *testing.T) {
	rows, err := snapshotRows(&Snapshot{PartitionedIndexes: []PartitionedIndex{{
		SchemaName: "s", RelName: "t", IndexRelName: "i", IndexRelID: 7,
		KeyColumns: []string{"k"}, IsValid: true}}})
	if err != nil {
		t.Fatalf("snapshotRows: %v", err)
	}
	var data string
	for _, r := range rows {
		if r.Category == "partitioned_indexes" {
			data = string(r.Data)
		}
	}
	if !strings.Contains(data, `"indexrelname":"i"`) ||
		!strings.Contains(data, `"key_columns":["k"]`) {
		t.Fatalf("partitioned_indexes row = %q, want the element persisted", data)
	}
	if delta, _ := snapstore.Coverage("partitioned_indexes"); !delta {
		t.Error("partitioned_indexes must be stored change-only, keyed by index name")
	}
	if !strings.Contains(partitionedIndexesSQL, "ic.oid > $1") ||
		!strings.Contains(partitionedIndexesSQL, "LIMIT $2") {
		t.Error("partitionedIndexesSQL must be keyset-paged and bounded")
	}
}
