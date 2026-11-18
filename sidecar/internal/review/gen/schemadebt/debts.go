package schemadebt

import (
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
)

// integerRank orders the integer types as format_type prints them.
var integerRank = map[string]int{"smallint": 1, "integer": 2, "bigint": 3}

// timeTypes are the types a time-ordered key has.
var timeTypes = map[string]bool{"timestamp with time zone": true,
	"timestamp without time zone": true, "date": true}

const typeTimestampNoTZ = "timestamp without time zone"

var (
	// rePrimaryKey is a single-column primary key as the schema section
	// lists it: "<name> PRIMARY KEY (<column>)".
	rePrimaryKey = regexp.MustCompile(`^\S+ PRIMARY KEY \(([^,()]+)\)$`)
	// reForeignKey is a single-column foreign key: "<name> FOREIGN KEY
	// (<column>) REFERENCES <table>(<column>)", then any actions.
	reForeignKey = regexp.MustCompile(
		`^(\S+) FOREIGN KEY \(([^,()]+)\) REFERENCES (\S+?)\(([^,()]+)\)`)
	// reIndexLead is an index definition's leading key column.
	reIndexLead = regexp.MustCompile(`USING \w+ \(([^,()\s]+)`)
)

// keyDebt is a single-column smallint or integer primary key.
type keyDebt struct {
	ref     ColumnRef
	table   workpack.TableFact
	colType string
}

// fkDebt is a foreign key column narrower than the key it references.
type fkDebt struct {
	child, parent         ColumnRef
	childType, parentType string
	childTab, parentTab   workpack.TableFact
	constraint            string
}

// tsDebt is a timestamp without time zone column of a table the window's
// top query families read.
type tsDebt struct {
	ref      ColumnRef
	table    workpack.TableFact
	families []string
}

// debts are the shapes the pack's schema section shows.
type debts struct {
	keys       []keyDebt
	fks        []fkDebt
	timestamps []tsDebt
}

// findDebts reads the shapes from the tables and the families reading them.
func findDebts(sch workpack.Schema, fams []workpack.Family) debts {
	byName := map[string]workpack.TableFact{}
	for _, t := range sch.Tables {
		byName[t.Table] = t
	}
	readers := map[string][]string{}
	for _, f := range fams {
		for _, rel := range f.Relations {
			readers[rel] = append(readers[rel], f.ID)
		}
	}
	var d debts
	for _, t := range sch.Tables {
		if col, typ, ok := primaryKey(t); ok && integerRank[typ] > 0 &&
			integerRank[typ] < integerRank["bigint"] {
			d.keys = append(d.keys, keyDebt{ref: ref(t.Table, col), table: t, colType: typ})
		}
		d.fks = append(d.fks, foreignKeys(t, byName)...)
		if ids := readers[t.Table]; len(ids) > 0 {
			for _, c := range t.Columns {
				if c.Type == typeTimestampNoTZ {
					d.timestamps = append(d.timestamps, tsDebt{ref: ref(t.Table, c.Name),
						table: t, families: ids})
				}
			}
		}
	}
	return d
}

// columns are the columns whose catalog facts the debts need, sorted.
func (d debts) columns() []ColumnRef {
	var out []ColumnRef
	add := func(c ColumnRef) {
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	for _, k := range d.keys {
		add(k.ref)
	}
	for _, f := range d.fks {
		add(f.parent)
		add(f.child)
	}
	for _, t := range d.timestamps {
		add(t.ref)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// primaryKey is the table's single-column primary key and its type.
func primaryKey(t workpack.TableFact) (col, typ string, ok bool) {
	for _, c := range t.Constraints {
		if m := rePrimaryKey.FindStringSubmatch(c); m != nil {
			col = unquoteIdent(m[1])
			typ, ok = columnType(t, col)
			return col, typ, ok
		}
	}
	return "", "", false
}

// foreignKeys are t's integer foreign key columns narrower than the
// integer key they reference.
func foreignKeys(t workpack.TableFact, byName map[string]workpack.TableFact) []fkDebt {
	var out []fkDebt
	for _, c := range t.Constraints {
		m := reForeignKey.FindStringSubmatch(c)
		if m == nil {
			continue
		}
		parent, ok := resolveTable(m[3], t.Table, byName)
		if !ok {
			continue
		}
		childCol, parentCol := unquoteIdent(m[2]), unquoteIdent(m[4])
		ct, okC := columnType(t, childCol)
		pt, okP := columnType(parent, parentCol)
		if !okC || !okP || integerRank[ct] == 0 || integerRank[ct] >= integerRank[pt] {
			continue
		}
		out = append(out, fkDebt{child: ref(t.Table, childCol),
			parent: ref(parent.Table, parentCol), childType: ct, parentType: pt,
			childTab: t, parentTab: parent, constraint: m[1]})
	}
	return out
}

// resolveTable finds a referenced table: pg_get_constraintdef leaves a
// table on the search path unqualified, so an unqualified name is looked
// up in the child's schema, then in public.
func resolveTable(name, child string, byName map[string]workpack.TableFact,
) (workpack.TableFact, bool) {
	parts := strings.Split(name, ".")
	for i, p := range parts {
		parts[i] = unquoteIdent(p)
	}
	if len(parts) == 2 {
		t, ok := byName[parts[0]+"."+parts[1]]
		return t, ok
	}
	schema, _, _ := strings.Cut(child, ".")
	for _, s := range []string{schema, "public"} {
		if t, ok := byName[s+"."+parts[0]]; ok {
			return t, true
		}
	}
	return workpack.TableFact{}, false
}

// timeOrderedKey is a date or timestamp column that leads the primary key
// or an index of t ("" for none).
func timeOrderedKey(t workpack.TableFact) string {
	if col, typ, ok := primaryKey(t); ok && timeTypes[typ] {
		return col
	}
	for _, def := range t.Indexes {
		if m := reIndexLead.FindStringSubmatch(def); m != nil {
			col := unquoteIdent(m[1])
			if typ, ok := columnType(t, col); ok && timeTypes[typ] {
				return col
			}
		}
	}
	return ""
}

func columnType(t workpack.TableFact, col string) (string, bool) {
	for _, c := range t.Columns {
		if c.Name == col {
			return c.Type, true
		}
	}
	return "", false
}

func hasColumn(t workpack.TableFact, names ...string) (string, bool) {
	for _, c := range t.Columns {
		if slices.Contains(names, strings.ToLower(c.Name)) {
			return c.Name, true
		}
	}
	return "", false
}

// ref is the column of a schema.table name.
func ref(table, column string) ColumnRef {
	schema, name, _ := strings.Cut(table, ".")
	return ColumnRef{Schema: schema, Table: name, Column: column}
}

// unquoteIdent undoes quote_ident: "Ab""c" is Ab"c; a bare name is as is.
func unquoteIdent(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return s
}
