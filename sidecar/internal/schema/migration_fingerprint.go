package schema

import "strings"

// catalogFingerprintSQL is the md5 of what the migration steps shape in the
// sage schema (config-04b): relations, columns, indexes with their
// validity and column options, constraints with their validation,
// triggers, functions with their settings (a pinned search_path) and view
// rules. It reads raw catalog columns only: the deparse functions
// (pg_get_expr, pg_get_viewdef) lock the relations they describe, and the
// ledger read must not wait behind a session holding a table lock. Day
// partitions are left out (retention adds and drops them every day);
// their parent carries the shape. Rows are not part of it. The deferred
// indexes are left out (config-04c): the background builder, not a step,
// owns them, so their builds and failures do not distrust the ledger.
var catalogFingerprintSQL = `/* pg_sage catalog fingerprint v2 */
SELECT md5(COALESCE(string_agg(line, E'\n' ORDER BY line), '')) FROM (
    SELECT 'relation ' || c.relname || ' ' || c.relkind::text AS line
      FROM pg_catalog.pg_class c
     WHERE c.relnamespace = 'sage'::regnamespace
       AND c.relkind IN ('r', 'p', 'v', 'm', 'S') AND NOT c.relispartition
    UNION ALL
    SELECT 'column ' || c.relname || '.' || a.attname || ' ' ||
           pg_catalog.format_type(a.atttypid, a.atttypmod) || ' ' || a.attnotnull::text ||
           ' ' || COALESCE(md5(d.adbin::text), '-')
      FROM pg_catalog.pg_class c
      JOIN pg_catalog.pg_attribute a
        ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
      LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid = c.oid AND d.adnum = a.attnum
     WHERE c.relnamespace = 'sage'::regnamespace
       AND c.relkind IN ('r', 'p', 'v', 'm') AND NOT c.relispartition
    UNION ALL
    SELECT 'index ' || i.relname || ' ON ' || c.relname || ' ' || x.indisunique::text ||
           ' ' || x.indisvalid::text || ' ' || x.indkey::text || ' ' || x.indclass::text ||
           ' ' || x.indoption::text ||
           ' ' || i.relam::text || ' ' || COALESCE(md5(x.indexprs::text), '-') ||
           ' ' || COALESCE(md5(x.indpred::text), '-')
      FROM pg_catalog.pg_index x
      JOIN pg_catalog.pg_class i ON i.oid = x.indexrelid
      JOIN pg_catalog.pg_class c ON c.oid = x.indrelid
     WHERE c.relnamespace = 'sage'::regnamespace AND NOT c.relispartition
       AND i.relname NOT IN ` + deferredIndexNamesSQL() + `
    UNION ALL
    SELECT 'constraint ' || c.relname || '.' || k.conname || ' ' || k.contype::text ||
           ' ' || k.convalidated::text || ' ' || COALESCE(k.conkey::text, '-') ||
           ' ' || k.confrelid::text || ' ' || COALESCE(k.confkey::text, '-') ||
           ' ' || k.confupdtype::text || k.confdeltype::text ||
           ' ' || COALESCE(md5(k.conbin::text), '-')
      FROM pg_catalog.pg_constraint k
      JOIN pg_catalog.pg_class c ON c.oid = k.conrelid
     WHERE c.relnamespace = 'sage'::regnamespace AND NOT c.relispartition
    UNION ALL
    SELECT 'trigger ' || c.relname || '.' || t.tgname || ' ' || t.tgfoid::text ||
           ' ' || t.tgtype::text || ' ' || t.tgenabled::text ||
           ' ' || md5(t.tgargs::text) || ' ' || COALESCE(md5(t.tgqual::text), '-')
      FROM pg_catalog.pg_trigger t
      JOIN pg_catalog.pg_class c ON c.oid = t.tgrelid
     WHERE c.relnamespace = 'sage'::regnamespace AND NOT t.tgisinternal
       AND NOT c.relispartition
    UNION ALL
    SELECT 'function ' || p.oid::pg_catalog.regprocedure::text || ' ' ||
           md5(COALESCE(p.prosrc, '')) || ' ' || p.provolatile::text || ' ' ||
           p.prosecdef::text || ' ' || COALESCE(p.proconfig::text, '-')
      FROM pg_catalog.pg_proc p
     WHERE p.pronamespace = 'sage'::regnamespace
    UNION ALL
    SELECT 'rule ' || c.relname || '.' || r.rulename || ' ' || md5(r.ev_action::text)
      FROM pg_catalog.pg_rewrite r
      JOIN pg_catalog.pg_class c ON c.oid = r.ev_class
     WHERE c.relnamespace = 'sage'::regnamespace
) fingerprint`

// catalogInvalidIndexSQL lists the INVALID sage indexes (a killed CREATE
// INDEX CONCURRENTLY leaves one): the boot then does not trust the ledger,
// and the step that builds the index, if any, rebuilds it. The deferred
// indexes are the background builder's (config-04c) and left out.
var catalogInvalidIndexSQL = `/* pg_sage catalog invalid index v2 */
SELECT COALESCE(array_agg(c.relname::text ORDER BY c.relname), '{}')
  FROM pg_catalog.pg_index x
  JOIN pg_catalog.pg_class c ON c.oid = x.indexrelid
 WHERE c.relnamespace = 'sage'::regnamespace AND NOT x.indisvalid
   AND c.relname NOT IN ` + deferredIndexNamesSQL()

// catalogNotValidSQL lists each NOT VALID sage constraint as its name and
// its table ("name sage.t"), for steps that validate one on a later boot.
const catalogNotValidSQL = `/* pg_sage catalog not valid v1 */
SELECT COALESCE(array_agg(k.conname::text || ' sage.' || c.relname::text
                          ORDER BY k.conname), '{}')
  FROM pg_catalog.pg_constraint k
  JOIN pg_catalog.pg_class c ON c.oid = k.conrelid
 WHERE c.relnamespace = 'sage'::regnamespace AND NOT k.convalidated
   AND NOT c.relispartition`

// validatesPending reports whether sql validates one of the NOT VALID
// constraints ("name sage.table"): it says VALIDATE CONSTRAINT and names
// the constraint or its table (a DO block that validates by catalog
// lookup). Such a step runs again until the constraint is validated.
func validatesPending(sql string, notValid []string) bool {
	if !strings.Contains(strings.ToUpper(sql), "VALIDATE CONSTRAINT") {
		return false
	}
	for _, entry := range notValid {
		name, table, _ := strings.Cut(entry, " ")
		if namesIdentifier(sql, name) || namesIdentifier(sql, table) {
			return true
		}
	}
	return false
}

// namesIdentifier reports whether sql holds name as a whole identifier.
func namesIdentifier(sql, name string) bool {
	if name == "" {
		return false
	}
	isIdent := func(b byte) bool {
		return b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
	}
	for from := 0; ; {
		i := strings.Index(sql[from:], name)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(name)
		if (start == 0 || !isIdent(sql[start-1])) && (end == len(sql) || !isIdent(sql[end])) {
			return true
		}
		from = start + 1
	}
}
