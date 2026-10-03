package autonomy

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// cloneFamily is a synthetic clone-schema family: n schemas with one
// parent and `children` child tables each, every child holding an
// unindexed foreign key. All DDL is unqualified (search_path), so no
// statement text names a member schema.
type cloneFamily struct {
	schemas  []string
	tag      string
	children int
}

func (f cloneFamily) parent() string { return "p_" + f.tag }

func (f cloneFamily) child(i int) string { return fmt.Sprintf("c%02d_%s", i, f.tag) }

func (f cloneFamily) childTargets() []string {
	targets := make([]string, 0, len(f.schemas)*f.children)
	for _, schema := range f.schemas {
		for i := 1; i <= f.children; i++ {
			targets = append(targets, schema+"."+f.child(i))
		}
	}
	return targets
}

// createCloneFamily builds a family whose children hold unindexed foreign
// keys. Building the primary keys scans the new tables, so the family has
// scan activity once the statistics are flushed.
func createCloneFamily(
	t *testing.T, pool *pgxpool.Pool, prefix string, n, children int,
) cloneFamily {
	t.Helper()
	return buildFamily(t, pool, prefix, n, children, func(f cloneFamily) string {
		ddl := fmt.Sprintf("CREATE TABLE %s (id bigint PRIMARY KEY); ", f.parent())
		for c := 1; c <= children; c++ {
			ddl += fmt.Sprintf("CREATE TABLE %s (id bigint PRIMARY KEY, p_id bigint "+
				"REFERENCES %s(id)); ", f.child(c), f.parent())
		}
		return ddl
	})
}

// createTextFamily builds a family of index-free tables, each with one
// type-tightening invariant (account_id stored as text). Nothing scans or
// writes them, so their activity counters stay exactly zero: an idle
// family whatever the statistics flush timing.
func createTextFamily(
	t *testing.T, pool *pgxpool.Pool, prefix string, n, tables int,
) cloneFamily {
	t.Helper()
	return buildFamily(t, pool, prefix, n, tables, func(f cloneFamily) string {
		ddl := ""
		for c := 1; c <= tables; c++ {
			ddl += fmt.Sprintf("CREATE TABLE %s (account_id text, n int); ", f.child(c))
		}
		return ddl
	})
}

func buildFamily(
	t *testing.T, pool *pgxpool.Pool, prefix string, n, children int,
	tables func(cloneFamily) string,
) cloneFamily {
	t.Helper()
	ctx := context.Background()
	tag := fmt.Sprintf("%x", time.Now().UnixNano())
	family := cloneFamily{tag: tag[len(tag)-8:], children: children}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire DDL connection: %v", err)
	}
	defer conn.Release()
	t.Cleanup(func() {
		for _, schema := range family.schemas {
			_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		}
	})
	for i := 1; i <= n; i++ {
		schema := fmt.Sprintf("%s_%06d", prefix, i)
		ddl := fmt.Sprintf("CREATE SCHEMA %s; SET search_path TO %s; ", schema, schema) +
			tables(family) + "RESET search_path"
		if _, err := conn.Exec(ctx, ddl); err != nil {
			t.Fatalf("create clone schema %s: %v", schema, err)
		}
		family.schemas = append(family.schemas, schema)
	}
	return family
}

func familyItems(items []schemaguard.Invariant, family cloneFamily) []schemaguard.Invariant {
	members := map[string]bool{}
	for _, schema := range family.schemas {
		members[schema] = true
	}
	result := []schemaguard.Invariant{}
	for _, item := range items {
		if members[item.Schema] {
			result = append(result, item)
		}
	}
	return result
}

func detectFamily(
	t *testing.T, detector schemaguard.Detector, family cloneFamily,
) []schemaguard.Invariant {
	t.Helper()
	items, err := detector.Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	return familyItems(items, family)
}

func TestFamilyDetectorMarksUnusedCopiesIdle(t *testing.T) {
	pool := requireAutonomyDB(t)
	family := createTextFamily(t, pool, "sgfidle", 5, 2)
	detector := newFamilyDetector(pool, postgresSchemaDetector{pool}, SchemaGuardOptions{})
	items := detectFamily(t, detector, family)
	if len(items) != 10 {
		t.Fatalf("family invariants = %d, want 5 schemas x 2 text id columns", len(items))
	}
	for _, item := range items {
		if item.Family == nil || !item.Family.Idle || len(item.Family.Members) != 5 ||
			!strings.Contains(item.Family.Reason, "no scans or writes") {
			t.Fatalf("invariant %s family = %+v, want an idle 5-schema family",
				item.Target(), item.Family)
		}
		if item.Subject != "account_id" || item.Kind != schemaguard.InvariantTypeTightening {
			t.Fatalf("invariant = %+v, want the account_id column as subject", item)
		}
	}
}

func TestFamilyDetectorKeepsLockedFamilyLive(t *testing.T) {
	pool := requireAutonomyDB(t)
	family := createTextFamily(t, pool, "sgflock", 5, 1)
	release := holdFamilyLock(t, pool, family, family.child(1))
	defer release()
	detector := newFamilyDetector(pool, postgresSchemaDetector{pool}, SchemaGuardOptions{})
	items := detectFamily(t, detector, family)
	if len(items) != 5 {
		t.Fatalf("family invariants = %d, want 5", len(items))
	}
	for _, item := range items {
		if item.Family == nil || item.Family.Idle ||
			!strings.Contains(item.Family.Reason, "locks") {
			t.Fatalf("locked family = %+v, want live because a session holds locks",
				item.Family)
		}
	}
}

// holdFamilyLock keeps an ACCESS SHARE lock on the first member's table
// in an open transaction until release is called.
func holdFamilyLock(
	t *testing.T, pool *pgxpool.Pool, family cloneFamily, table string,
) func() {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock transaction: %v", err)
	}
	// Unqualified (SET LOCAL search_path): only pg_locks, not a statement
	// text naming the schema, shows the family is in use.
	if _, err := tx.Exec(ctx, "SET LOCAL search_path TO "+family.schemas[0]+
		"; LOCK TABLE "+table+" IN ACCESS SHARE MODE"); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("lock family table: %v", err)
	}
	var once sync.Once
	return func() { once.Do(func() { _ = tx.Rollback(context.Background()) }) }
}

func TestFamilyDetectorTurnsQuietFamilyIdleAfterTheWindow(t *testing.T) {
	pool := requireAutonomyDB(t)
	family := createTextFamily(t, pool, "sgfwin", 5, 1)
	writeToFamily(t, pool, family)
	clock := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	detector := newFamilyDetector(pool, postgresSchemaDetector{pool}, SchemaGuardOptions{
		IdleWindow: time.Hour, Now: func() time.Time { return clock }})
	first := detectFamily(t, detector, family)
	if len(first) == 0 || first[0].Family == nil || first[0].Family.Idle ||
		!strings.Contains(first[0].Family.Reason, "scanned or written") {
		t.Fatalf("freshly written family = %+v, want live", first)
	}
	clock = clock.Add(2 * time.Hour)
	second := detectFamily(t, detector, family)
	if len(second) == 0 || second[0].Family == nil || !second[0].Family.Idle {
		t.Fatalf("family unchanged for 2h with a 1h window = %+v, want idle", second)
	}
}

// writeToFamily inserts into every member (unqualified) and waits until the
// cumulative statistics show the writes in every member.
func writeToFamily(t *testing.T, pool *pgxpool.Pool, family cloneFamily) {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire write connection: %v", err)
	}
	defer conn.Release()
	var version int
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&version); err != nil {
		t.Fatalf("server version: %v", err)
	}
	for _, schema := range family.schemas {
		if version >= 150000 {
			if _, err := conn.Exec(ctx, "SELECT pg_stat_force_next_flush()"); err != nil {
				t.Fatalf("force stats flush: %v", err)
			}
		}
		if _, err := conn.Exec(ctx, "SET search_path TO "+schema+"; INSERT INTO "+
			family.child(1)+" VALUES ('1', 1); RESET search_path"); err != nil {
			t.Fatalf("write to %s: %v", schema, err)
		}
	}
	waitFor(t, 15*time.Second, func() bool {
		shapes, err := loadSchemaShapes(ctx, pool)
		if err != nil {
			t.Fatalf("loadSchemaShapes: %v", err)
		}
		written := 0
		for _, shape := range shapes {
			if containsName(family.schemas, shape.Schema) && shape.Activity > 0 {
				written++
			}
		}
		return written == len(family.schemas)
	})
}

func TestLoadSchemaShapesListsSortedTables(t *testing.T) {
	pool := requireAutonomyDB(t)
	family := createCloneFamily(t, pool, "sgfshape", 1, 2)
	shapes, err := loadSchemaShapes(context.Background(), pool)
	if err != nil {
		t.Fatalf("loadSchemaShapes: %v", err)
	}
	for _, shape := range shapes {
		if shape.Schema == "sage" || shape.Schema == "pg_catalog" {
			t.Fatalf("system schema %s listed", shape.Schema)
		}
		if shape.Schema != family.schemas[0] {
			continue
		}
		want := []string{family.child(1), family.child(2), family.parent()}
		if strings.Join(shape.Tables, ",") != strings.Join(want, ",") {
			t.Fatalf("tables = %v, want %v", shape.Tables, want)
		}
		return
	}
	t.Fatalf("schema %s missing from shapes", family.schemas[0])
}
