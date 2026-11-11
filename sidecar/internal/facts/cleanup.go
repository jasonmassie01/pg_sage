package facts

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// CategoryTestFixtureCleanup is one confirmed test-fixture fact's cleanup
// batch: the matching schemas and the script that drops them, for an
// operator to review and run. pg_sage never drops a schema itself.
const CategoryTestFixtureCleanup = "test_fixture_cleanup"

// maxCleanupSchemas bounds one cleanup batch.
const maxCleanupSchemas = 500

// cleanupLockTimeout bounds each drop's wait for a lock.
const cleanupLockTimeout = "5s"

// schemaActivityExpr is a schema's activity counter, the fixture
// detector's quiet signal: scans and row writes summed over its tables
// (aliases c and s of schemaActivityFrom).
const schemaActivityExpr = `COALESCE(sum(COALESCE(s.seq_scan, 0) + COALESCE(s.idx_scan, 0) +
                    COALESCE(s.n_tup_ins, 0) + COALESCE(s.n_tup_upd, 0) +
                    COALESCE(s.n_tup_del, 0)), 0)::bigint`

const schemaActivityFrom = `FROM pg_catalog.pg_namespace n
LEFT JOIN pg_catalog.pg_class c ON c.relnamespace = n.oid AND c.relkind IN ('r', 'p')
LEFT JOIN pg_catalog.pg_stat_all_tables s ON s.relid = c.oid`

// schemaActivitySQL reads the named schemas' activity counters (a schema
// missing from the result no longer exists).
const schemaActivitySQL = `/* pg_sage facts */
SELECT n.nspname::text, ` + schemaActivityExpr + `
` + schemaActivityFrom + `
WHERE n.nspname = ANY($1)
GROUP BY n.nspname`

// dropBlockBody is one schema's DO block (its name literal and baseline
// are filled in): it skips the schema when its activity moved since the
// batch was built or another session holds a lock in it, and when the
// drop itself waits past lock_timeout. It is indented so no script line
// starts with BEGIN.
const dropBlockBody = `  DECLARE
    v_schema text := %s;
    v_baseline bigint := %d;
    v_cur bigint;
  BEGIN
    SELECT ` + schemaActivityExpr + `
      INTO v_cur
    ` + schemaActivityFrom + `
    WHERE n.nspname = v_schema;
    IF v_cur IS DISTINCT FROM v_baseline THEN
      RAISE NOTICE 'skip %%: activity or stats reset since the batch was built', v_schema;
      RETURN;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_locks l
               JOIN pg_catalog.pg_class lc ON lc.oid = l.relation
               JOIN pg_catalog.pg_namespace ns ON ns.oid = lc.relnamespace
               WHERE l.locktype = 'relation' AND ns.nspname = v_schema
                 AND l.pid IS DISTINCT FROM pg_catalog.pg_backend_pid()
                 AND l.database = (SELECT d.oid FROM pg_catalog.pg_database d
                                   WHERE d.datname = pg_catalog.current_database())) THEN
      RAISE NOTICE 'skip %%: another session holds a lock on one of its relations', v_schema;
      RETURN;
    END IF;
    EXECUTE 'DROP SCHEMA IF EXISTS ' || pg_catalog.quote_ident(v_schema) || ' CASCADE';
    RAISE NOTICE 'dropped %%', v_schema;
  EXCEPTION WHEN lock_not_available THEN
    RAISE NOTICE 'skip %%: lock_timeout reached waiting for a lock', v_schema;
  END`

// cleanupFinding is a test-fixture fact's cleanup batch over the excluded
// schemas it matches that have a baseline in activity (a schema without
// one is left for a later batch).
func cleanupFinding(fact Fact, excluded []string, activity map[string]int64,
	now time.Time) (analyzer.Finding, bool) {
	p, err := ParsePattern(fact.Kind, fact.Subject)
	if err != nil {
		return analyzer.Finding{}, false
	}
	var schemas []string
	for _, s := range excluded {
		if _, ok := activity[s]; ok && globMatch(p.Schema, s) {
			schemas = append(schemas, s)
		}
	}
	if len(schemas) == 0 {
		return analyzer.Finding{}, false
	}
	return analyzer.Finding{
		Category: CategoryTestFixtureCleanup, Severity: "info", ObjectType: "schema",
		ObjectIdentifier: fact.Subject,
		Title: fmt.Sprintf("%d test-fixture schemas match %s (fact #%d): cleanup batch "+
			"ready", len(schemas), fact.Subject, fact.ID),
		Detail: map[string]any{"fact_id": fact.ID, "pattern": fact.Subject,
			"schemas": schemas, "schema_count": len(schemas),
			"generated_at": now.UTC().Format(time.RFC3339),
			"cleanup_sql":  cleanupSQL(fact.ID, fact.Subject, schemas, activity, now),
			"provenance":   fact.Provenance()},
		Recommendation: "These schemas are confirmed test fixtures (" + fact.Provenance() +
			"): pg_sage excludes them from findings and budgets. Review the batch and " +
			"run it in psql autocommit to drop them; each schema is re-checked for " +
			"activity and locks first. pg_sage never drops schemas itself.",
	}, true
}

// cleanupSQL is the batch script: one DO block per schema (at most
// maxCleanupSchemas; the rest wait for the next batch), each its own
// transaction when run in psql autocommit.
func cleanupSQL(factID int64, pattern string, schemas []string,
	baselines map[string]int64, at time.Time) string {
	n := min(len(schemas), maxCleanupSchemas)
	var b strings.Builder
	fmt.Fprintf(&b, "-- pg_sage cleanup batch for fact #%d (%s): %d of %d matching "+
		"schemas, generated %s\n", factID, commentSafe(pattern), n, len(schemas),
		at.UTC().Format(time.RFC3339))
	b.WriteString("-- Run in psql autocommit, one statement at a time; never wrap it in " +
		"BEGIN or use -1.\n-- Each block drops one schema in its own transaction and " +
		"skips it (NOTICE) when it\n-- saw activity since the batch was built or " +
		"another session holds a lock in it.\n")
	fmt.Fprintf(&b, "SET lock_timeout = '%s';\n", cleanupLockTimeout)
	for _, s := range schemas[:n] {
		b.WriteString(dropBlock(s, baselines[s]))
	}
	if len(schemas) > n {
		fmt.Fprintf(&b, "-- %d more schemas follow in the next batch\n", len(schemas)-n)
	}
	return b.String()
}

// dropBlock is one schema's DO statement, dollar-quoted with a tag its
// body does not contain.
func dropBlock(schema string, baseline int64) string {
	body := fmt.Sprintf(dropBlockBody, quoteLiteral(schema), baseline)
	tag := "$sage$"
	for i := 1; strings.Contains(body, tag); i++ {
		tag = fmt.Sprintf("$sage%d$", i)
	}
	return "\nDO " + tag + "\n" + body + "\n" + tag + ";\n"
}

// commentSafe is s on one line, so it cannot end a "--" comment.
func commentSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// quoteLiteral is s as a SQL string literal whatever
// standard_conforming_strings says (an E-string when s holds a backslash).
func quoteLiteral(s string) string {
	q := "'" + strings.ReplaceAll(s, "'", "''") + "'"
	if strings.Contains(s, `\`) {
		return "E" + strings.ReplaceAll(q, `\`, `\\`)
	}
	return q
}
