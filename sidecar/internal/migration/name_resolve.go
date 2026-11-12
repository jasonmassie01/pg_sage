package migration

import (
	"context"
	"strings"
)

// candidateSchemasSQL lists the user schemas holding a table-like
// relation of the name (tables, partitioned tables, materialized views).
const candidateSchemasSQL = `
SELECT n.nspname
FROM   pg_catalog.pg_class c
JOIN   pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE  c.relname = $1
  AND  c.relkind IN ('r', 'p', 'm')
  AND  n.nspname <> 'information_schema'
  AND  n.nspname NOT LIKE 'pg\_%'
ORDER  BY n.nspname`

// roleSearchPathSQL reads the search_path settings that apply to role in
// this database, most specific first (database+role, role, database).
// pg_db_role_setting is readable on managed providers.
const roleSearchPathSQL = `
SELECT s.setconfig
FROM   pg_catalog.pg_db_role_setting s
WHERE  s.setdatabase IN (0, (SELECT oid FROM pg_catalog.pg_database
                              WHERE datname = current_database()))
  AND  (s.setrole = 0 OR s.setrole =
        (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = $1))
ORDER  BY (s.setrole <> 0) DESC, (s.setdatabase <> 0) DESC`

// defaultSearchPath is PostgreSQL's search_path when no role or database
// setting applies.
var defaultSearchPath = []string{"$user", "public"}

// resolveTableSchema fills risk.SchemaName for an unqualified table: the
// DDL role's search_path, then public, then a unique relname. Several
// matches mark the name ambiguous (stats unknown). With no setting the
// server default ("$user", public) applies. A failed candidate read
// leaves the name unresolved, which falls back to public.
func (ra *RiskAssessor) resolveTableSchema(
	ctx context.Context, risk *DDLRisk, role string,
) {
	candidates, err := ra.candidateSchemas(ctx, risk.TableName)
	if err != nil {
		risk.noteReadErr(err)
		ra.logFn("debug", "migration: schema lookup failed for %s: %v",
			risk.TableName, err)
		return
	}
	path, err := ra.roleSearchPath(ctx, role)
	if err != nil {
		ra.logFn("debug", "migration: search_path lookup failed for role %q: %v",
			role, err)
	}
	if path == nil {
		path = defaultSearchPath
	}
	schema, ambiguous := resolveSchema(candidates, path, role)
	risk.SchemaName, risk.NameAmbiguous = schema, ambiguous
}

func (ra *RiskAssessor) candidateSchemas(
	ctx context.Context, table string,
) ([]string, error) {
	rows, err := ra.catalog().Query(ctx, candidateSchemasSQL, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (ra *RiskAssessor) roleSearchPath(
	ctx context.Context, role string,
) ([]string, error) {
	rows, err := ra.catalog().Query(ctx, roleSearchPathSQL, role)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var path []string
	for rows.Next() {
		var config []string
		if err := rows.Scan(&config); err != nil {
			return nil, err
		}
		if p := parseSearchPath(config); path == nil && p != nil {
			path = p
		}
	}
	return path, rows.Err()
}

// parseSearchPath returns the schemas of the search_path entry of a
// setconfig array, unquoted, or nil when it has none.
func parseSearchPath(config []string) []string {
	for _, entry := range config {
		value, ok := strings.CutPrefix(entry, "search_path=")
		if !ok {
			continue
		}
		var out []string
		for _, part := range strings.Split(value, ",") {
			part = strings.Trim(strings.TrimSpace(part), `"`)
			if part != "" {
				out = append(out, part)
			}
		}
		return out
	}
	return nil
}

// resolveSchema picks the schema of an unqualified table among the
// schemas that hold it: the first search_path entry ("$user" is role,
// pg_temp is never chosen), then public, then the only candidate. Two or
// more candidates otherwise are ambiguous.
func resolveSchema(candidates, path []string, role string) (string, bool) {
	has := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		if !strings.HasPrefix(c, "pg_temp") {
			has[c] = true
		}
	}
	for _, s := range path {
		if s == "$user" {
			s = role
		}
		if s != "" && has[s] {
			return s, false
		}
	}
	if has["public"] {
		return "public", false
	}
	switch len(has) {
	case 0:
		return "", false
	case 1:
		for s := range has {
			return s, false
		}
	}
	return "", true
}
