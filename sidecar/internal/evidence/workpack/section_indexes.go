package workpack

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// indexes.json: every index of the user schemas from pg_index, with the
// window's scans and its table's writes from the snapshots, and the
// exact-duplicate and left-prefix overlaps computed from pg_index's keys,
// operator classes, collations, options, expressions and predicate. No
// advice: which overlap matters is review-02's call.

// userSchemas keeps pg_sage's and the system schemas out of a catalog read
// on alias n (pg_namespace).
const userSchemas = `n.nspname NOT IN ('sage', 'information_schema')
   AND n.nspname NOT LIKE 'pg\_%'`

// indexRow is one index as pg_index describes it. keys holds every
// indkey entry (key columns, then INCLUDE columns); classes, colls and
// opts hold the key columns' only. total is how many indexes the read
// counted before its bound.
type indexRow struct {
	schema, table, name, def, method, exprs, pred string
	size                                          int64
	unique, primary, constraint, valid, partial   bool
	keyAtts, total                                int
	keys, classes, colls, opts                    []string
}

var indexesSQL = `/* pg_sage */ SELECT n.nspname, ct.relname, ci.relname, am.amname,
       pg_get_indexdef(ci.oid), pg_relation_size(ci.oid), i.indisunique, i.indisprimary,
       i.indisvalid, i.indpred IS NOT NULL,
       EXISTS (SELECT 1 FROM pg_constraint c WHERE c.conindid = ci.oid
                  AND c.conrelid = i.indrelid AND c.contype IN ('p', 'u', 'x')),
       i.indnkeyatts, i.indkey::text, i.indclass::text, i.indcollation::text,
       i.indoption::text, COALESCE(pg_get_expr(i.indexprs, i.indrelid), ''),
       COALESCE(pg_get_expr(i.indpred, i.indrelid), ''), count(*) OVER ()
  FROM pg_index i
  JOIN pg_class ci ON ci.oid = i.indexrelid
  JOIN pg_class ct ON ct.oid = i.indrelid
  JOIN pg_namespace n ON n.oid = ci.relnamespace
  JOIN pg_am am ON am.oid = ci.relam
 WHERE ` + userSchemas + `
 ORDER BY n.nspname, ci.relname
 LIMIT ` + fmt.Sprint(MaxIndexes+1)

func readIndexes(ctx context.Context, tx pgx.Tx, _ catalogInput, cat *catalogData) error {
	rows, err := tx.Query(ctx, indexesSQL)
	if err != nil {
		return err
	}
	cat.indexes, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (indexRow, error) {
		var x indexRow
		var keys, classes, colls, opts string
		err := row.Scan(&x.schema, &x.table, &x.name, &x.method, &x.def, &x.size, &x.unique,
			&x.primary, &x.valid, &x.partial, &x.constraint, &x.keyAtts, &keys, &classes,
			&colls, &opts, &x.exprs, &x.pred, &x.total)
		x.keys, x.classes = strings.Fields(keys), strings.Fields(classes)
		x.colls, x.opts = strings.Fields(colls), strings.Fields(opts)
		return x, err
	})
	return err
}

// keyColumn is one key position: column, operator class, collation and
// option.
func (x indexRow) keyColumn(i int) string {
	at := func(s []string) string {
		if i < len(s) {
			return s[i]
		}
		return ""
	}
	return at(x.keys) + "/" + at(x.classes) + "/" + at(x.colls) + "/" + at(x.opts)
}

func (x indexRow) qualified() string { return x.schema + "." + x.name }

// family is what two indexes must share to overlap at all.
func (x indexRow) family() string {
	return strings.Join([]string{x.schema, x.table, x.method, x.pred}, "\x00")
}

// signature is everything that defines an index's content.
func (x indexRow) signature() string {
	return strings.Join([]string{x.family(), x.exprs, strings.Join(x.keys, " "),
		strings.Join(x.classes, " "), strings.Join(x.colls, " "),
		strings.Join(x.opts, " ")}, "\x00")
}

// isPrefixOf: x's key columns lead y's, x is shorter, both index plain
// columns only, and x has no INCLUDE columns.
func (x indexRow) isPrefixOf(y indexRow) bool {
	if x.family() != y.family() || x.exprs != "" || y.exprs != "" ||
		x.keyAtts >= y.keyAtts || x.keyAtts == 0 || len(x.keys) != x.keyAtts {
		return false
	}
	for i := range x.keyAtts {
		if x.keyColumn(i) != y.keyColumn(i) {
			return false
		}
	}
	return true
}

// indexOverlaps lists, per "schema.index", the exact duplicates and the
// indexes it is a left prefix of, each sorted.
func indexOverlaps(rows []indexRow) (dup, prefix map[string][]string) {
	dup, prefix = map[string][]string{}, map[string][]string{}
	for i, x := range rows {
		for j, y := range rows {
			if i == j {
				continue
			}
			if x.signature() == y.signature() {
				dup[x.qualified()] = append(dup[x.qualified()], y.qualified())
			} else if x.isPrefixOf(y) {
				prefix[x.qualified()] = append(prefix[x.qualified()], y.qualified())
			}
		}
	}
	for _, m := range []map[string][]string{dup, prefix} {
		for k := range m {
			sort.Strings(m[k])
		}
	}
	return dup, prefix
}

// buildIndexes is the section from the catalog rows and the window.
func buildIndexes(rows []indexRow, truncated bool, win relWindow) Indexes {
	out := Indexes{Indexes: []IndexFact{}, Total: len(rows), ScansStatus: win.status}
	if len(rows) > 0 && rows[0].total > out.Total {
		out.Total = rows[0].total
	}
	if len(rows) > MaxIndexes {
		rows, truncated = rows[:MaxIndexes], true
	}
	out.Truncated = truncated
	dup, prefix := indexOverlaps(rows)
	for _, x := range rows {
		f := IndexFact{ID: "index/idx/" + x.qualified(), Schema: x.schema, Table: x.table,
			Index: x.name, Definition: x.def, Method: x.method, SizeBytes: x.size,
			Unique: x.unique, Primary: x.primary, ConstraintBacking: x.constraint,
			Valid: x.valid, Partial: x.partial, Scans: valueUnknown,
			TableWrites: valueUnknown, DuplicateOf: orEmpty(dup[x.qualified()]),
			PrefixOf: orEmpty(prefix[x.qualified()])}
		if win.status == relStatusOK {
			f.Scans = scansOf(win, x.qualified())
			f.TableWrites = writesOf(win, x.schema+"."+x.table)
		}
		out.Indexes = append(out.Indexes, f)
	}
	return out
}

func scansOf(win relWindow, key string) any {
	a, okA := win.idxFirst[key]
	b, okB := win.idxLast[key]
	if !okA || !okB || b < a {
		return valueUnknown
	}
	return b - a
}

func writesOf(win relWindow, key string) any {
	a, okA := win.tabFirst[key]
	b, okB := win.tabLast[key]
	if !okA || !okB || b.ins < a.ins || b.upd < a.upd || b.del < a.del {
		return valueUnknown
	}
	return Writes{Inserts: b.ins - a.ins, Updates: b.upd - a.upd, Deletes: b.del - a.del}
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
