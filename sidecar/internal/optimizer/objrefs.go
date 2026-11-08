package optimizer

import "strings"

// ObjectKind is the kind of a schema-qualified object, other than a
// function, that an index expression names.
type ObjectKind int

const (
	// ObjectType is a type named by a cast (x::s.t, CAST(x AS s.t)) or a
	// typed literal (s.t 'v'). A domain's CHECK and a cast's function run
	// on every value the index stores, during the build too.
	ObjectType ObjectKind = iota + 1
	// ObjectOperator is an operator named by OPERATOR(s.op), which binds
	// its schema whatever the search_path.
	ObjectOperator
)

func (k ObjectKind) String() string {
	if k == ObjectOperator {
		return "operator"
	}
	return "type"
}

// ObjectRef is a type or operator a CREATE INDEX's key list or predicate
// names with a schema other than pg_catalog. Like a qualified call, it
// binds that schema whatever the search_path, so another role that can
// create objects there could have planted it.
type ObjectRef struct {
	Kind         ObjectKind
	Schema, Name string
}

func (r ObjectRef) String() string { return r.Kind.String() + " " + r.Schema + "." + r.Name }

// IndexObjectRefs returns the types and operators a CREATE INDEX names
// with a schema other than pg_catalog, in order. It reads the statement as
// IndexFunctionCalls does, with the same errors.
func IndexObjectRefs(ddl string) ([]ObjectRef, error) {
	_, objects, err := scanIndexExpressions(ddl)
	return objects, err
}

// addObject records a qualified type or operator; bare names and
// pg_catalog's resolve only in pg_catalog under the executor's pin.
func (cs *callScanner) addObject(kind ObjectKind, parts []namePart, name string) {
	if len(parts) == 0 {
		return
	}
	schema := parts[len(parts)-1].text
	if schema == "pg_catalog" {
		return
	}
	cs.objects = append(cs.objects, ObjectRef{Kind: kind, Schema: schema, Name: name})
}

// operatorChars are the characters of a PostgreSQL operator name.
const operatorChars = "+-*/<>=~!@#%^&|`?"

// operator reads OPERATOR( [schema .] op ), with cs.pos at "(". A form it
// cannot read is an error, never a guess.
func (cs *callScanner) operator() error {
	cs.pos++
	cs.skipSpace()
	var qualifier []namePart
	for cs.pos < cs.end && (cs.src[cs.pos] == '"' || isNameStart(cs.src[cs.pos])) {
		part, err := cs.namePart()
		if err != nil {
			return err
		}
		qualifier = []namePart{part}
		cs.skipSpace()
		if cs.pos >= cs.end || cs.src[cs.pos] != '.' {
			return unreadable("an OPERATOR() without an operator")
		}
		cs.pos++
		cs.skipSpace()
	}
	start := cs.pos
	for cs.pos < cs.end && strings.IndexByte(operatorChars, cs.src[cs.pos]) >= 0 {
		cs.pos++
	}
	op := cs.src[start:cs.pos]
	cs.skipSpace()
	if op == "" || cs.pos >= cs.end || cs.src[cs.pos] != ')' {
		return unreadable("an OPERATOR() form")
	}
	cs.pos++
	cs.addObject(ObjectOperator, qualifier, op)
	cs.operand = false
	return nil
}
