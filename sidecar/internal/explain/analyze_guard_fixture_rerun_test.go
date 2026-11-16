package explain

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// env-cgo-c: each guard check runs twice on one server, once with recorded
// shapes (TestGuardCatalog*) and once with the parser (TestGuardParser*,
// cgo). A cleanup that leaves an object behind makes the second setup fail
// with "already exists", so every fixture must set up, clean up and set up
// again, leaving the catalog as it found it.
//
// No concurrent access test: fixtures are serial DDL on one database; the
// guard's own concurrency is TestGuardCatalogConcurrent/TestGuardParserConcurrent.

// egObjectsSQL lists the fixtures' objects: every eg_ relation, type,
// function, schema and fixture operator.
const egObjectsSQL = `SELECT kind || ':' || name FROM (
	SELECT 'class' AS kind, n.nspname || '.' || c.relname AS name
		FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relname LIKE 'eg\_%'
	UNION ALL SELECT 'type', n.nspname || '.' || t.typname
		FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace
		WHERE t.typname LIKE 'eg\_%' OR t.typname LIKE '\_eg\_%'
	UNION ALL SELECT 'function', n.nspname || '.' || p.proname
		FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
		WHERE p.proname LIKE 'eg\_%' OR (n.nspname = 'public' AND p.proname = 'upper')
	UNION ALL SELECT 'operator', n.nspname || '.' || o.oprname
		FROM pg_catalog.pg_operator o JOIN pg_catalog.pg_namespace n ON n.oid = o.oprnamespace
		WHERE o.oprname IN ('===', '=#=')
	UNION ALL SELECT 'schema', nspname FROM pg_catalog.pg_namespace WHERE nspname LIKE 'eg\_%'
) s ORDER BY 1`

func egObjects(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), egObjectsSQL)
	if err != nil {
		t.Fatalf("list fixture objects: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan fixture object: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list fixture objects: %v", err)
	}
	return out
}

// fixtureStep is one statement batch of a fixture's life cycle.
type fixtureStep struct{ name, sql string }

// guardFixtureLifecycles are the fixtures in the order the checks build
// and tear them down: the base guard fixture first, an extension on top.
func guardFixtureLifecycles() map[string][]fixtureStep {
	withBase := func(name, setup, cleanup string) []fixtureStep {
		return []fixtureStep{
			{"guard setup", guardFixtureSQL}, {name + " setup", setup},
			{name + " cleanup", cleanup}, {"guard cleanup", guardCleanupSQL},
		}
	}
	return map[string][]fixtureStep{
		"guard":  {{"guard setup", guardFixtureSQL}, {"guard cleanup", guardCleanupSQL}},
		"hidden": withBase("hidden", hiddenFixtureSQL, hiddenCleanupSQL),
		"cast":   withBase("cast", castFixtureSQL, castCleanupSQL),
		"domain": withBase("domain", domainFixtureSQL, domainCleanupSQL),
	}
}

// runCleanups runs a life cycle's cleanup steps, so a failed round does
// not leave objects that fail the next subtest's setup.
func runCleanups(pool *pgxpool.Pool, steps []fixtureStep) {
	for _, s := range steps {
		if strings.HasSuffix(s.name, "cleanup") {
			_, _ = pool.Exec(context.Background(), s.sql)
		}
	}
}

func TestGuardFixturesRerunAndCleanUpCompletely(t *testing.T) {
	pool := liveExplainPool(t)
	for name, steps := range guardFixtureLifecycles() {
		t.Run(name, func(t *testing.T) {
			before := egObjects(t, pool)
			t.Cleanup(func() { runCleanups(pool, steps) })
			for round := 1; round <= 2; round++ {
				for _, s := range steps {
					if _, err := pool.Exec(context.Background(), s.sql); err != nil {
						t.Fatalf("round %d, %s: %v", round, s.name, err)
					}
				}
			}
			after := egObjects(t, pool)
			if strings.Join(after, ",") != strings.Join(before, ",") {
				t.Errorf("fixture %s left objects behind:\nbefore %v\nafter  %v",
					name, before, after)
			}
		})
	}
}

// A run killed before its cleanup leaves the fixture in place; the next
// run's setup must replace it, not fail on it.
func TestGuardFixtureSetupsReplaceALeftover(t *testing.T) {
	pool := liveExplainPool(t)
	for name, steps := range guardFixtureLifecycles() {
		t.Run(name, func(t *testing.T) {
			before := egObjects(t, pool)
			t.Cleanup(func() { runCleanups(pool, steps) })
			last := 0
			for i, s := range steps {
				if strings.HasSuffix(s.name, "setup") {
					last = i
				}
			}
			again := append(append([]fixtureStep{}, steps[:last+1]...), steps[last:]...)
			for _, s := range again {
				if _, err := pool.Exec(context.Background(), s.sql); err != nil {
					t.Fatalf("%s: %v", s.name, err)
				}
			}
			if after := egObjects(t, pool); strings.Join(after, ",") != strings.Join(before, ",") {
				t.Errorf("fixture %s left objects behind:\nbefore %v\nafter  %v",
					name, before, after)
			}
		})
	}
}

// A check that fails midway still cleans up: the cast check drops its
// cast before its last assertion, and the cleanup it registered must still
// succeed and remove the table afterwards (the double drop is harmless).
func TestCastCleanupIsIdempotent(t *testing.T) {
	pool := liveExplainPool(t)
	before := egObjects(t, pool)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), castCleanupSQL) })
	for _, s := range []fixtureStep{{"cast setup", castFixtureSQL},
		{"cast cleanup", castCleanupSQL}, {"cast cleanup again", castCleanupSQL}} {
		if _, err := pool.Exec(context.Background(), s.sql); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
	}
	if after := egObjects(t, pool); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Errorf("cast fixture left objects behind:\nbefore %v\nafter  %v", before, after)
	}
}
