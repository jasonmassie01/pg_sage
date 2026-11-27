package migration

import (
	"regexp"
	"strconv"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// lockRank orders lock levels; an unknown level ranks lowest.
var lockRank = map[string]int{
	lockShareUpdateExclusive: 1, "SHARE": 2, lockShareRowExclusive: 3,
	"EXCLUSIVE": 4, lockAccessExclusive: 5,
}

// strongerLock returns the stronger of two lock levels.
func strongerLock(a, b string) string {
	if lockRank[b] > lockRank[a] {
		return b
	}
	return a
}

// mergeRules appends each classification whose rule is not already in
// found: like the regex rules, a statement matches a rule at most once,
// and the first match keeps its column and type.
func mergeRules(found, more []DDLClassification) []DDLClassification {
	for _, c := range more {
		dup := false
		for _, f := range found {
			dup = dup || f.RuleID == c.RuleID
		}
		if !dup {
			found = append(found, c)
		}
	}
	return found
}

// stringItems returns the String values of a node list, skipping others.
func stringItems(items []*pg_query.Node) []string {
	var out []string
	for _, n := range items {
		if s := n.GetString_(); s != nil {
			out = append(out, s.GetSval())
		}
	}
	return out
}

// defOption reports whether the option list sets name to true, as
// PostgreSQL's defGetBoolean reads it: no argument means true.
func defOption(opts []*pg_query.Node, name string) bool {
	for _, n := range opts {
		d := n.GetDefElem()
		if d == nil || !strings.EqualFold(d.GetDefname(), name) {
			continue
		}
		return defBoolean(d.GetArg())
	}
	return false
}

func defBoolean(arg *pg_query.Node) bool {
	switch {
	case arg == nil:
		return true
	case arg.GetBoolean() != nil:
		return arg.GetBoolean().GetBoolval()
	case arg.GetInteger() != nil:
		return arg.GetInteger().GetIval() != 0
	case arg.GetString_() != nil:
		switch strings.ToLower(arg.GetString_().GetSval()) {
		case "false", "off", "no", "0":
			return false
		}
	}
	return true
}

// typeNames is a type's name without the pg_catalog prefix the parser
// adds to SQL-standard spellings (int, varchar, timestamp ...).
func typeNames(t *pg_query.TypeName) []string {
	names := stringItems(t.GetNames())
	if len(names) == 2 && names[0] == "pg_catalog" {
		return names[1:]
	}
	return names
}

// typeText renders a type for refineAlterType: its name, integer
// modifiers and array bounds ("varchar(20)", "numeric(12,2)", "text[]").
func typeText(t *pg_query.TypeName) string {
	if t == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(strings.Join(typeNames(t), "."))
	var mods []string
	for _, n := range t.GetTypmods() {
		if iv := n.GetAConst().GetIval(); iv != nil {
			mods = append(mods, strconv.Itoa(int(iv.GetIval())))
		}
	}
	if len(mods) > 0 {
		b.WriteString("(" + strings.Join(mods, ",") + ")")
	}
	for range t.GetArrayBounds() {
		b.WriteString("[]")
	}
	return b.String()
}

// builtinTypes are the type names (after typeNames) of built-in scalar
// types. A user type could be a domain with a CHECK, which makes ADD
// COLUMN rewrite the table, so only these are a known safe form.
var builtinTypes = map[string]bool{
	"int2": true, "int4": true, "int8": true, "float4": true, "float8": true,
	"numeric": true, "bool": true, "boolean": true, "text": true, "varchar": true,
	"bpchar": true, "char": true, "bytea": true, "date": true, "time": true,
	"timetz": true, "timestamp": true, "timestamptz": true, "interval": true,
	"uuid": true, "json": true, "jsonb": true, "inet": true, "cidr": true,
	"macaddr": true, "macaddr8": true, "bit": true, "varbit": true,
	"integer": true, "bigint": true, "smallint": true, "real": true,
}

func builtinType(t *pg_query.TypeName) bool {
	names := typeNames(t)
	return len(names) == 1 && builtinTypes[names[0]]
}

// isSerialType: serial columns get a nextval() default, which fills every
// existing row and so rewrites the table.
func isSerialType(t *pg_query.TypeName) bool {
	names := typeNames(t)
	if len(names) != 1 {
		return false
	}
	switch names[0] {
	case "serial", "serial2", "serial4", "serial8", "smallserial", "bigserial":
		return true
	}
	return false
}

// volatileExpr reports whether an expression calls a function that is not
// in the stable/immutable default allowlist (immutableDefaults). A
// schema-qualified call other than pg_catalog's can name any function, so
// it counts as volatile.
func volatileExpr(n *pg_query.Node) bool {
	if n == nil {
		return false
	}
	volatile := false
	walkMessages(n.ProtoReflect(), func(msg protoreflect.Message) bool {
		if fc, ok := msg.Interface().(*pg_query.FuncCall); ok {
			names := stringItems(fc.GetFuncname())
			if len(names) == 2 && names[0] == "pg_catalog" {
				names = names[1:]
			}
			volatile = len(names) != 1 || !immutableDefaults[names[0]]
		}
		return !volatile
	})
	return volatile
}

// walkMessages visits msg and every message under it until visit
// returns false.
func walkMessages(msg protoreflect.Message, visit func(protoreflect.Message) bool) bool {
	if !visit(msg) {
		return false
	}
	keep := true
	msg.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		switch {
		case field.IsList() && field.Message() != nil:
			list := value.List()
			for i := 0; i < list.Len() && keep; i++ {
				keep = walkMessages(list.Get(i).Message(), visit)
			}
		case field.Message() != nil && !field.IsMap():
			keep = walkMessages(value.Message(), visit)
		}
		return keep
	})
	return keep
}

// reNotNullConstraint is PG 18's ALTER TABLE t ADD CONSTRAINT n NOT NULL
// col [NO INHERIT] [NOT VALID], which the PG 17 grammar of the bundled
// parser cannot read.
var reNotNullConstraint = regexp.MustCompile(`(?i)^ALTER TABLE (?:IF EXISTS )?(?:ONLY )?` +
	qualifiedRE + ` ADD CONSTRAINT ` + identRE + ` NOT NULL (` + identRE + `)` +
	`(?: NO INHERIT)?( NOT VALID)?(?: NO INHERIT)?$`)

// notNullConstraintText classifies the PG 18 NOT NULL constraint form
// from its text when the parser rejected it. ok is false for any other
// text, and before PG 18 (where the server rejects it too).
func notNullConstraintText(stmt string, major int) ([]DDLClassification, bool) {
	m := reNotNullConstraint.FindStringSubmatch(stmt)
	if m == nil {
		return nil, false
	}
	am := astMapper{stmt: stmt, major: major}
	found, ok := am.notNullConstraint(m[4] != "", []string{unquoteIdent(m[3])})
	for i := range found {
		if m[1] != "" {
			found[i].SchemaName = unquoteIdent(m[1])
		}
		found[i].TableName = unquoteIdent(m[2])
	}
	return found, ok
}
