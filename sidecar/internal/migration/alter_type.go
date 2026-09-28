package migration

import (
	"context"
	"regexp"
	"strconv"
	"strings"
)

const columnTypeSQL = `
SELECT pg_catalog.format_type(a.atttypid, a.atttypmod)
FROM   pg_catalog.pg_attribute a
JOIN   pg_catalog.pg_class c ON c.oid = a.attrelid
JOIN   pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE  n.nspname = $1 AND c.relname = $2 AND a.attname = $3
  AND  a.attnum > 0 AND NOT a.attisdropped`

// refineAlterType marks ALTER COLUMN ... TYPE as metadata-only when the
// change is binary coercible and PostgreSQL skips the rewrite
// (varchar(n) -> varchar(m >= n), varchar -> text, G7-B35).
func (a *Advisor) refineAlterType(ctx context.Context, c *DDLClassification) {
	if c.RuleID != "ddl_alter_type_rewrite" || a.pool == nil ||
		c.TableName == "" || c.ColumnName == "" || c.TargetType == "" {
		return
	}
	var current string
	err := a.pool.QueryRow(ctx, columnTypeSQL,
		schemaOrPublic(c.SchemaName), c.TableName, c.ColumnName,
	).Scan(&current)
	if err != nil {
		a.logFn("debug", "migration: column type lookup failed: %v", err)
		return
	}
	if isBinaryCoercibleChange(current, c.TargetType) {
		c.RequiresRewrite = false
		c.Description = "Binary-coercible type change: no rewrite, " +
			"but ACCESS EXCLUSIVE is still taken"
	}
}

var reVarchar = regexp.MustCompile(
	`^(?:character varying|varchar)\s*(?:\(\s*(\d+)\s*\))?$`)

// isBinaryCoercibleChange reports whether changing a column from
// current (format_type output) to target (as written in the DDL) is a
// no-rewrite change. Only the common varchar/text widenings are
// recognized; anything else is conservatively treated as a rewrite.
func isBinaryCoercibleChange(current, target string) bool {
	cur := strings.ToLower(strings.TrimSpace(current))
	tgt := strings.ToLower(strings.TrimSpace(target))
	curVC := reVarchar.FindStringSubmatch(cur)
	if curVC == nil && cur != "text" {
		return false
	}
	if tgt == "text" {
		return true
	}
	tgtVC := reVarchar.FindStringSubmatch(tgt)
	if tgtVC == nil {
		return false
	}
	if tgtVC[1] == "" {
		return true // unbounded varchar accepts every text value
	}
	if curVC == nil || curVC[1] == "" {
		return false // text/varchar -> varchar(n) must check lengths
	}
	curLen, errCur := strconv.Atoi(curVC[1])
	tgtLen, errTgt := strconv.Atoi(tgtVC[1])
	return errCur == nil && errTgt == nil && tgtLen >= curLen
}
