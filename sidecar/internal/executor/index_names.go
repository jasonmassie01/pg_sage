package executor

import (
	"strings"
)

// extractIndexName parses the index name from a CREATE INDEX statement.
func extractIndexName(sql string) string {
	upper := strings.ToUpper(sql)
	idx := strings.Index(upper, "INDEX")
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(sql[idx+5:])
	// Skip optional "CONCURRENTLY" and "IF NOT EXISTS".
	upper = strings.ToUpper(rest)
	if strings.HasPrefix(upper, "CONCURRENTLY") {
		rest = strings.TrimSpace(rest[len("CONCURRENTLY"):])
		upper = strings.ToUpper(rest)
	}
	if strings.HasPrefix(upper, "IF NOT EXISTS") {
		rest = strings.TrimSpace(rest[len("IF NOT EXISTS"):])
	} else if strings.HasPrefix(upper, "IF EXISTS") {
		// DROP INDEX CONCURRENTLY IF EXISTS <name>
		rest = strings.TrimSpace(rest[len("IF EXISTS"):])
	}
	// Next token is the index name.
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	if strings.EqualFold(fields[0], "ON") {
		return ""
	}
	name := strings.Trim(fields[0], "\"")
	return name
}

// unqualifyIndexName reduces a possibly schema-qualified, quoted, or
// semicolon-terminated index reference to a bare lowercase name for
// comparison (e.g. `public."idx_x";` -> `idx_x`).
func unqualifyIndexName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.TrimSuffix(name, ";")
	name = strings.Trim(name, "\"")
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return strings.ToLower(strings.Trim(name, "\""))
}

// isSelfReferentialDrop reports whether dropDDL drops the same index that
// createSQL creates. The optimizer reuses a recommendation's rollback DDL
// (a DROP of the NEW index) as Detail["drop_ddl"], so the INCLUDE-upgrade
// path must skip it — otherwise it would immediately drop the index it just
// created. A genuine supersede targets a different, pre-existing index.
func isSelfReferentialDrop(createSQL, dropDDL string) bool {
	c := unqualifyIndexName(extractIndexName(createSQL))
	d := unqualifyIndexName(extractIndexName(dropDDL))
	return c != "" && c == d
}
