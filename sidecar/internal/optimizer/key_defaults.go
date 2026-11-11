package optimizer

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// KeyDefault is what PostgreSQL applies to an index key on one column
// when the DDL names no collation or operator class, and what
// pg_get_indexdef then omits (indexes-01). Each entry lists the canonical
// spellings of a name: bare, and schema-qualified unless in pg_catalog.
type KeyDefault struct {
	Collation []string            // the column's own collation; nil when not collatable
	Opclass   map[string][]string // access method -> its default operator class
}

// keyDefaultRow is one catalog row: a column's collation and one default
// operator class whose input type is the column's type (exact) or one it
// is binary compatible with (preferred: the preferred type of the
// column's type category). method is "" when the column has none.
type keyDefaultRow struct {
	column, collation, collationSchema string
	method, opclass, opclassSchema     string
	exact, preferred                   bool
}

// keyDefaultsSQL lists every column of one table with its collation and
// the default operator classes that could apply to it: one for its own
// type (a domain's base type), one it is binary coercible to, or a
// polymorphic one (anyarray, anyenum, anyrange) that matches it.
const keyDefaultsSQL = `/* pg_sage optimizer */ WITH cols AS (
	SELECT a.attname, a.attcollation, b.oid AS typ, b.typtype, b.typcategory
	FROM pg_catalog.pg_attribute a
	JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
	JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
	JOIN pg_catalog.pg_type t ON t.oid = a.atttypid
	JOIN pg_catalog.pg_type b ON b.oid = CASE WHEN t.typtype = 'd' THEN t.typbasetype
		ELSE t.oid END
	WHERE n.nspname = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped)
SELECT cols.attname, COALESCE(co.collname, ''), COALESCE(con.nspname, ''),
	COALESCE(am.amname, ''), COALESCE(oc.opcname, ''), COALESCE(ocn.nspname, ''),
	COALESCE(oc.opcintype = cols.typ, false),
	COALESCE(it.typispreferred AND it.typcategory = cols.typcategory, false)
FROM cols
LEFT JOIN pg_catalog.pg_collation co ON co.oid = cols.attcollation
LEFT JOIN pg_catalog.pg_namespace con ON con.oid = co.collnamespace
LEFT JOIN pg_catalog.pg_opclass oc ON oc.opcdefault AND (oc.opcintype = cols.typ
	OR EXISTS (SELECT 1 FROM pg_catalog.pg_cast k WHERE k.castsource = cols.typ
		AND k.casttarget = oc.opcintype AND k.castmethod = 'b' AND k.castcontext = 'i')
	OR (oc.opcintype = 'pg_catalog.anyarray'::pg_catalog.regtype AND cols.typcategory = 'A')
	OR (oc.opcintype = 'pg_catalog.anyenum'::pg_catalog.regtype AND cols.typtype = 'e')
	OR (oc.opcintype = 'pg_catalog.anyrange'::pg_catalog.regtype AND cols.typtype = 'r'))
LEFT JOIN pg_catalog.pg_type it ON it.oid = oc.opcintype
LEFT JOIN pg_catalog.pg_am am ON am.oid = oc.opcmethod
LEFT JOIN pg_catalog.pg_namespace ocn ON ocn.oid = oc.opcnamespace
ORDER BY 1, 4, 5`

// fetchKeyDefaults reads the key defaults of every column of
// schema.table, bounded by the reader's statement and lock timeouts.
func fetchKeyDefaults(ctx context.Context, q catalogread.Querier, schema, table string,
) (map[string]KeyDefault, error) {
	rows, err := q.Query(ctx, keyDefaultsSQL, schema, table)
	if err != nil {
		return nil, fmt.Errorf("read key defaults of %s.%s: %w", schema, table, err)
	}
	defer rows.Close()
	var all []keyDefaultRow
	for rows.Next() {
		var r keyDefaultRow
		if err := rows.Scan(&r.column, &r.collation, &r.collationSchema, &r.method,
			&r.opclass, &r.opclassSchema, &r.exact, &r.preferred); err != nil {
			return nil, fmt.Errorf("scan key defaults of %s.%s: %w", schema, table, err)
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read key defaults of %s.%s: %w", schema, table, err)
	}
	return resolveKeyDefaults(all), nil
}

// resolveKeyDefaults picks each column's default operator class per
// access method the way PostgreSQL's GetDefaultOpClass does: the one for
// the exact type, else the only compatible one of the category's
// preferred type (varchar: text_ops, not bpchar_ops), else the only
// compatible one. Ambiguous candidates resolve to none, so an explicit
// name stays part of the key (the safe direction: not a duplicate, not
// covered).
func resolveKeyDefaults(rows []keyDefaultRow) map[string]KeyDefault {
	out := make(map[string]KeyDefault)
	picks := map[string]map[string]*opclassPick{}
	for _, r := range rows {
		if _, seen := out[r.column]; !seen {
			out[r.column] = KeyDefault{Collation: spellings(r.collationSchema, r.collation),
				Opclass: map[string][]string{}}
			picks[r.column] = map[string]*opclassPick{}
		}
		if r.method == "" {
			continue
		}
		p := picks[r.column][r.method]
		if p == nil {
			p = &opclassPick{}
			picks[r.column][r.method] = p
		}
		p.add(r)
	}
	for col, byMethod := range picks {
		for method, p := range byMethod {
			if r, ok := p.chosen(); ok {
				out[col].Opclass[method] = spellings(r.opclassSchema, r.opclass)
			}
		}
	}
	return out
}

// opclassPick collects one column's default operator class candidates
// for one access method.
type opclassPick struct{ exact, preferred, compatible []keyDefaultRow }

func (p *opclassPick) add(r keyDefaultRow) {
	switch {
	case r.exact:
		p.exact = append(p.exact, r)
	case r.preferred:
		p.preferred = append(p.preferred, r)
	default:
		p.compatible = append(p.compatible, r)
	}
}

func (p *opclassPick) chosen() (keyDefaultRow, bool) {
	switch {
	case len(p.exact) > 0:
		return p.exact[0], len(p.exact) == 1
	case len(p.preferred) > 0:
		return p.preferred[0], len(p.preferred) == 1
	case len(p.compatible) == 1:
		return p.compatible[0], true
	}
	return keyDefaultRow{}, false
}

// spellings are the canonical ways a DDL can name schema.name (the scanner
// drops a pg_catalog qualifier); nil for no name.
func spellings(schema, name string) []string {
	if name == "" {
		return nil
	}
	bare := canonicalIdent(name)
	if schema == "" || schema == "pg_catalog" {
		return []string{bare}
	}
	return []string{bare, canonicalIdent(schema) + "." + bare}
}
