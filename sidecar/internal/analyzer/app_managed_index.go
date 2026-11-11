package analyzer

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// CategoryAppManagedIndex is an index pg_sage dropped that came back with
// the same definition: the application owns it (dogfood lifeos-1:
// public.idx_thesis_allocation_run was dropped 8 times in 40 minutes and
// recreated by the application's migrations each time).
const CategoryAppManagedIndex = "app_managed_index"

// appManagedIndex is the drop history of one index that came back.
type appManagedIndex struct {
	Drops    int
	LastDrop time.Time
	// Schema, Name and Table are the index and its table (in the index's
	// schema) as the catalog spells them, exact case.
	Schema, Name, Table string
	// Qualified reports that a drop named the index's schema; an
	// unqualified drop was resolved through the search_path.
	Qualified bool
}

// dropProposalCategories are the findings that propose dropping an index.
var dropProposalCategories = map[string]bool{"duplicate_index": true, "unused_index": true}

// dropNamePattern matches a DROP INDEX target: one or two parts, each a
// quoted identifier ("" escapes a quote) or a run of characters that is no
// space, semicolon, dot or quote. Text that is no name (an unterminated
// quote) does not match, so to_regclass never sees bad syntax (PG14 and
// PG15 raise on it).
const dropNamePattern = `(?:"(?:[^"]|"")+"|[^\s;."]+)(?:\.(?:"(?:[^"]|"")+"|[^\s;."]+))?`

// appManagedSQL lists pg_sage's executed index drops of the last 180 days
// whose index exists again: the drop's target text, the index and its
// table as the catalog names them, and the index's current definition.
// Drops pg_sage rolled back itself are not the application's doing.
const appManagedSQL = `/* pg_sage analyzer */
SELECT m[3], ix.nspname, ix.relname, ix.tablename, al.rollback_sql, al.executed_at,
       ix.def
FROM sage.action_log al,
     LATERAL pg_catalog.regexp_match(al.sql_executed,
         '^\s*DROP\s+INDEX\s+(CONCURRENTLY\s+)?(IF\s+EXISTS\s+)?(` + dropNamePattern + `)',
         'i') m,
     LATERAL (SELECT n.nspname::text, c.relname::text, t.relname::text AS tablename,
                     pg_catalog.pg_get_indexdef(c.oid) AS def
              FROM pg_catalog.pg_index i
              JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid
              JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
              JOIN pg_catalog.pg_class t ON t.oid = i.indrelid
              WHERE i.indexrelid = pg_catalog.to_regclass(m[3])) ix
WHERE al.action_type = 'drop_index' AND al.rollback_sql IS NOT NULL
  AND al.outcome NOT IN ('rolled_back', 'rolling_back', 'failed', 'pending')
  AND al.executed_at > pg_catalog.now() - interval '180 days'`

// loadAppManagedIndexes returns, by lower-cased schema.index (the form
// markAppManaged looks findings up by, whatever the drop's quoting), the
// indexes pg_sage dropped that exist again with the definition they had.
func loadAppManagedIndexes(ctx context.Context, pool catalogread.Querier) (
	map[string]appManagedIndex, error) {
	rows, err := pool.Query(ctx, appManagedSQL)
	if err != nil {
		return nil, fmt.Errorf("read index drop history: %w", err)
	}
	defer rows.Close()
	out := map[string]appManagedIndex{}
	for rows.Next() {
		var target, schema, name, table, rollback, current string
		var at time.Time
		if err := rows.Scan(&target, &schema, &name, &table, &rollback, &at,
			&current); err != nil {
			return nil, fmt.Errorf("scan index drop history: %w", err)
		}
		if normalizeIndexDefinition(current) != normalizeIndexDefinition(rollback) {
			continue
		}
		key := strings.ToLower(schema + "." + name)
		m := out[key]
		m.Drops++
		if m.Drops == 1 || at.After(m.LastDrop) {
			m.LastDrop, m.Schema, m.Name, m.Table = at, schema, name, table
		}
		m.Qualified = m.Qualified || dropNamesSchema(target)
		out[key] = m
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read index drop history: %w", err)
	}
	return out, nil
}

// dropNamesSchema reports whether a DROP INDEX target names a schema: a
// dot outside double quotes.
func dropNamesSchema(target string) bool {
	quoted := false
	for _, c := range target {
		switch {
		case c == '"':
			quoted = !quoted // a doubled quote toggles twice
		case c == '.' && !quoted:
			return true
		}
	}
	return false
}

var indexDefinitionNoise = regexp.MustCompile(`(?i)\b(CONCURRENTLY|IF\s+NOT\s+EXISTS)\b`)

// normalizeIndexDefinition compares CREATE INDEX statements ignoring
// CONCURRENTLY, IF NOT EXISTS, case, a trailing semicolon and spacing.
func normalizeIndexDefinition(sql string) string {
	s := strings.TrimSuffix(strings.TrimSpace(sql), ";")
	s = indexDefinitionNoise.ReplaceAllString(s, " ")
	s = strings.Join(strings.Fields(strings.ToLower(s)), " ")
	return strings.ReplaceAll(strings.ReplaceAll(s, ", ", ","), "( ", "(")
}

// markAppManaged replaces the drop proposals for app-managed indexes with
// one app_managed_index finding per index: no SQL, so nothing proposes
// or executes the drop again.
func markAppManaged(findings []Finding, managed map[string]appManagedIndex) []Finding {
	if len(managed) == 0 {
		return findings
	}
	out := make([]Finding, 0, len(findings))
	replaced := map[string][]string{}
	var order []string
	for _, f := range findings {
		key := strings.ToLower(f.ObjectIdentifier)
		if _, ok := managed[key]; !ok || !dropProposalCategories[f.Category] {
			out = append(out, f)
			continue
		}
		if _, seen := replaced[key]; !seen {
			order = append(order, f.ObjectIdentifier)
		}
		replaced[key] = append(replaced[key], f.Category)
	}
	for _, ident := range order {
		key := strings.ToLower(ident)
		out = append(out, appManagedFinding(ident, managed[key], replaced[key]))
	}
	return out
}

func appManagedFinding(ident string, m appManagedIndex, categories []string) Finding {
	sort.Strings(categories)
	return Finding{
		Category: CategoryAppManagedIndex, Severity: "info", ObjectType: "index",
		ObjectIdentifier: ident,
		Title: fmt.Sprintf("Index %s belongs to the application: pg_sage dropped it %d "+
			"times and it came back", ident, m.Drops),
		Detail: map[string]any{"drops": m.Drops, "last_drop": m.LastDrop,
			"replaced_categories": categories},
		Recommendation: "This index was recreated with the same definition after " +
			"pg_sage dropped it, most likely by the application's schema migrations. " +
			"pg_sage will not propose dropping it again. If it is redundant, remove it " +
			"in the application's migrations.",
		ActionRisk: "safe",
	}
}

// applyAppManaged loads the drop history and marks app-managed indexes.
// Without the history the findings pass unchanged and the category is
// not evaluated (its open findings stay as they are).
func (a *Analyzer) applyAppManaged(ctx context.Context, findings []Finding) []Finding {
	if a.pool == nil {
		return findings
	}
	managed, err := loadAppManagedIndexes(ctx, a.catalog())
	if err != nil {
		a.logFn("WARN", "analyzer: app-managed indexes: %v", err)
		return findings
	}
	a.eval.evaluated(CategoryAppManagedIndex)
	return markAppManaged(findings, managed)
}

// AppManagedIndex is the drop history of one index pg_sage dropped that
// came back with the same definition.
type AppManagedIndex = appManagedIndex

// AppManagedIndexes returns, by lower-cased schema.index, the indexes
// pg_sage dropped that exist again with the definition they had: the
// evidence the facts detector cites when it proposes that the application's
// migrations own them (roadmap 2.3).
func AppManagedIndexes(ctx context.Context, q catalogread.Querier) (
	map[string]AppManagedIndex, error) {
	return loadAppManagedIndexes(ctx, q)
}
