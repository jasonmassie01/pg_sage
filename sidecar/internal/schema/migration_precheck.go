package schema

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// Migration precheck (config-04). Before a migration step runs, each of its
// statements is classified. A statement whose effect the catalog already
// shows is a no-op and is left out, so a second boot does not take the
// ACCESS EXCLUSIVE lock ALTER TABLE ... ADD COLUMN IF NOT EXISTS takes
// before it checks, or the SHARE lock of CREATE INDEX IF NOT EXISTS. Every
// statement the classifier cannot prove done runs, in its original order,
// in one implicit transaction as before.

// stmtShape is what the classifier knows about one statement.
type stmtShape int

const (
	// shapeUnknown is a statement the classifier does not know: it always
	// runs, and migrationSteps must mark its step alwaysRun.
	shapeUnknown stmtShape = iota
	// shapeSetting is SET / SET LOCAL: it runs only with other work.
	shapeSetting
	// shapeColumns is ALTER TABLE sage.t ADD COLUMN IF NOT EXISTS ... only.
	shapeColumns
	// shapeIndex is CREATE [UNIQUE] INDEX IF NOT EXISTS n ON sage.t.
	shapeIndex
	// shapeRelation is CREATE TABLE / SEQUENCE IF NOT EXISTS sage.t.
	shapeRelation
	// shapeEveryBoot is a known statement with no catalog check that runs
	// on every boot without a DDL table lock: a DO block (catalog-guarded
	// by convention, proved by the lock tests), CREATE OR REPLACE FUNCTION
	// or VIEW, or DML backfill.
	shapeEveryBoot
)

// catalogCheck is one fact a statement's precheck reads from the catalog.
type catalogCheck struct {
	kind string // "column", "index" or "relation"
	rel  string // schema-qualified relation, e.g. sage.findings
	col  string // column name for kind "column"
}

// migrationStmt is one classified statement of a step.
type migrationStmt struct {
	sql    string
	shape  stmtShape
	checks []catalogCheck
	done   bool // set by markDone: the catalog shows every check holds
}

var (
	reAlterTable = regexp.MustCompile(`(?is)^ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?` +
		`(?:ONLY\s+)?(sage\.[a-z_][a-z0-9_]*)\s+(.*)$`)
	reAddColumn = regexp.MustCompile(`(?is)^ADD\s+(?:COLUMN\s+)?IF\s+NOT\s+EXISTS\s+` +
		`([a-z_][a-z0-9_]*)\s`)
	reCreateIndex = regexp.MustCompile(`(?is)^CREATE\s+(?:UNIQUE\s+)?INDEX\s+IF\s+NOT\s+` +
		`EXISTS\s+([a-z_][a-z0-9_]*)\s+ON\s+(?:ONLY\s+)?sage\.[a-z_][a-z0-9_]*\s*(?:\(|USING\s)`)
	reCreateRelation = regexp.MustCompile(`(?is)^CREATE\s+(?:UNLOGGED\s+)?(?:TABLE|SEQUENCE)` +
		`\s+IF\s+NOT\s+EXISTS\s+(sage\.[a-z_][a-z0-9_]*)[\s(]`)
	reSetting   = regexp.MustCompile(`(?is)^SET\s+(?:LOCAL\s+)?[a-z_]+\s*(?:=|TO)\s`)
	reEveryBoot = regexp.MustCompile(`(?is)^(?:DO\s|CREATE\s+OR\s+REPLACE\s+` +
		`(?:FUNCTION|VIEW)\s+sage\.|(?:UPDATE|INSERT\s+INTO|DELETE\s+FROM)\s+sage\.|WITH\s)`)
)

// classifyStatement returns the shape of one statement and, for the
// skippable shapes, the catalog facts that make it a no-op.
func classifyStatement(stmt string) migrationStmt {
	m := migrationStmt{sql: stmt}
	head := strings.TrimSpace(stripSQLComments(stmt)) + " "
	if alter := reAlterTable.FindStringSubmatch(head); alter != nil {
		m.checks = addColumnChecks(strings.ToLower(alter[1]), alter[2])
		if m.checks != nil {
			m.shape = shapeColumns
		}
		return m
	}
	if ix := reCreateIndex.FindStringSubmatch(head); ix != nil {
		m.shape = shapeIndex
		m.checks = []catalogCheck{{kind: "index", rel: "sage." + strings.ToLower(ix[1])}}
		return m
	}
	if rel := reCreateRelation.FindStringSubmatch(head); rel != nil {
		m.shape = shapeRelation
		m.checks = []catalogCheck{{kind: "relation", rel: strings.ToLower(rel[1])}}
		return m
	}
	switch {
	case reSetting.MatchString(head):
		m.shape = shapeSetting
	case reEveryBoot.MatchString(head):
		m.shape = shapeEveryBoot
	}
	return m
}

// addColumnChecks returns one column check per clause when every clause of
// the ALTER TABLE is ADD COLUMN IF NOT EXISTS, and nil otherwise.
func addColumnChecks(rel, clauses string) []catalogCheck {
	var checks []catalogCheck
	for _, clause := range splitTopLevel(clauses, ',') {
		add := reAddColumn.FindStringSubmatch(strings.TrimSpace(clause) + " ")
		if add == nil {
			return nil
		}
		checks = append(checks, catalogCheck{
			kind: "column", rel: rel, col: strings.ToLower(add[1]),
		})
	}
	return checks
}

// classifyStep splits a step's SQL into statements and classifies each.
// Statements holding only comments or whitespace are dropped.
func classifyStep(sql string) []migrationStmt {
	var out []migrationStmt
	for _, stmt := range splitSQL(sql) {
		if strings.TrimSpace(stripSQLComments(stmt)) == "" {
			continue
		}
		out = append(out, classifyStatement(stmt))
	}
	return out
}

// precheckSQL answers, in order, whether each catalog fact holds: a column
// that exists and is not dropped, a valid index, or an existing relation.
const precheckSQL = `/* pg_sage migration precheck v1 */
SELECT COALESCE(array_agg(present ORDER BY ord), '{}'::boolean[])
  FROM (SELECT c.ord, CASE c.kind
          WHEN 'column' THEN EXISTS (
               SELECT 1 FROM pg_catalog.pg_attribute a
                WHERE a.attrelid = pg_catalog.to_regclass(c.rel)
                  AND a.attname = c.col AND a.attnum > 0 AND NOT a.attisdropped)
          WHEN 'index' THEN EXISTS (
               SELECT 1 FROM pg_catalog.pg_index i
                WHERE i.indexrelid = pg_catalog.to_regclass(c.rel) AND i.indisvalid)
          WHEN 'relation' THEN pg_catalog.to_regclass(c.rel) IS NOT NULL
          ELSE false END AS present
          FROM unnest($1::text[], $2::text[], $3::text[])
               WITH ORDINALITY AS c(kind, rel, col, ord)) s`

// markDone reads the catalog facts of stmts in one query and records, per
// statement, whether the catalog shows it done. A statement with no facts
// is never done.
func markDone(ctx context.Context, db bootstrapDB, stmts []migrationStmt) error {
	var kinds, rels, cols []string
	for _, s := range stmts {
		for _, c := range s.checks {
			kinds, rels, cols = append(kinds, c.kind), append(rels, c.rel), append(cols, c.col)
		}
	}
	var present []bool
	if len(kinds) > 0 {
		if err := db.QueryRow(ctx, precheckSQL, kinds, rels, cols).Scan(&present); err != nil {
			return fmt.Errorf("reading the catalog: %w", err)
		}
		if len(present) != len(kinds) {
			return fmt.Errorf("catalog precheck answered %d of %d facts",
				len(present), len(kinds))
		}
	}
	next := 0
	for i := range stmts {
		done := len(stmts[i].checks) > 0
		for range stmts[i].checks {
			done = done && present[next]
			next++
		}
		stmts[i].done = done
	}
	return nil
}

// pendingWork reports whether a statement other than a setting is not done.
func pendingWork(stmts []migrationStmt) bool {
	for _, s := range stmts {
		if !s.done && s.shape != shapeSetting {
			return true
		}
	}
	return false
}

// hasChecks reports whether any of stmts has a catalog fact.
func hasChecks(stmts []migrationStmt) bool {
	for _, s := range stmts {
		if len(s.checks) > 0 {
			return true
		}
	}
	return false
}
