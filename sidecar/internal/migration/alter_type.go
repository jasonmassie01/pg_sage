package migration

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// columnTypeSQL reads a column's type. A collation other than the
// default (OID 100, the one ALTER TYPE gives text or varchar when it names
// none) is appended as COLLATE name: the change would reset it and rebuild
// the column's indexes, so that type is never binary coercible.
const columnTypeSQL = `
SELECT pg_catalog.format_type(a.atttypid, a.atttypmod) ||
       CASE WHEN a.attcollation IN (0, 100) THEN ''
            ELSE ' COLLATE ' || pg_catalog.quote_ident(co.collname) END
FROM   pg_catalog.pg_attribute a
JOIN   pg_catalog.pg_class c ON c.oid = a.attrelid
JOIN   pg_catalog.pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_catalog.pg_collation co ON co.oid = a.attcollation
WHERE  n.nspname = $1 AND c.relname = $2 AND a.attname = $3
  AND  a.attnum > 0 AND NOT a.attisdropped`

// refineAlterType marks ALTER COLUMN ... TYPE as metadata-only when the
// change is binary coercible and PostgreSQL skips the rewrite
// (varchar(n) -> varchar(m >= n), varchar -> text, G7-B35). It reads the
// column's current type through q (a bounded catalog read). A column that
// does not exist yet is left alone; a failed read is returned and the
// change stays a rewrite.
func refineAlterType(ctx context.Context, q catalogread.Querier, c *DDLClassification) error {
	if c.RuleID != "ddl_alter_type_rewrite" || q == nil ||
		c.TableName == "" || c.ColumnName == "" || c.TargetType == "" {
		return nil
	}
	var current string
	err := q.QueryRow(ctx, columnTypeSQL,
		schemaOrPublic(c.SchemaName), c.TableName, c.ColumnName,
	).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("column type lookup for %s.%s.%s failed: %w",
			schemaOrPublic(c.SchemaName), c.TableName, c.ColumnName, err)
	}
	if isBinaryCoercibleChange(current, c.TargetType) {
		c.RequiresRewrite = false
		c.Description = "Binary-coercible type change: no rewrite, " +
			"but ACCESS EXCLUSIVE is still taken"
	}
	return nil
}

// refineAlterType is the free function over the advisor's bounded
// catalog reads; a failed read is logged at debug and the change stays a
// rewrite.
func (a *Advisor) refineAlterType(ctx context.Context, c *DDLClassification) {
	if a.pool == nil {
		return
	}
	if err := refineAlterType(ctx, a.assessor.catalog(), c); err != nil {
		a.logFn("debug", "migration: %v", err)
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
