package optimizer

import (
	"regexp"
	"slices"
	"strings"
)

// keyBase is a key's column, or its expression without casts and
// grouping parentheses, with no decoration.
func keyBase(k indexKey) string {
	if k.expr != "" {
		return canonicalShape(k.expr)
	}
	return k.column
}

func keyBases(keys []indexKey) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = keyBase(k)
	}
	return out
}

// shapeKeys renders an index's keys for comparison (indexes-01): the key,
// then its collation and operator class unless they equal the column's
// defaults for method, then its ordering. A btree scans backward, so it
// reads as the uniform reversal that starts ascending: (a DESC, b DESC)
// renders as (a, b) and (a DESC) as (a), while (a, b DESC) keeps its mixed
// order. Other methods do not order, so order is not part of their keys.
func shapeKeys(keys []indexKey, method string, defaults map[string]KeyDefault) []string {
	ordered := method == "btree"
	invert := ordered && len(keys) > 0 && keys[0].decor.order == "desc"
	out := make([]string, len(keys))
	for i, k := range keys {
		s := keyIdentity(k, method, defaults)
		if ordered {
			s += orderSuffix(withoutDefaults(k, method, defaults), invert)
		}
		out[i] = s
	}
	return out
}

// keyIdentity renders a key without its ordering: the key, then its
// collation and operator class unless they are the column's defaults for
// method.
func keyIdentity(k indexKey, method string, defaults map[string]KeyDefault) string {
	d := withoutDefaults(k, method, defaults)
	s := keyBase(k)
	if d.collation != "" {
		s += " collate " + d.collation
	}
	if d.opclass != "" {
		s += " " + opclassShape(d.opclass)
	}
	return s
}

// quotedNumber is a quoted numeric literal in an operator class's
// parameter list.
var quotedNumber = regexp.MustCompile(`'(-?[0-9]+(?:\.[0-9]+)?)'`)

// opclassShape is an operator class with its numeric parameter values
// unquoted: pg_get_indexdef quotes every value (tsvector_ops
// (siglen='32')) while a DDL may not (tsvector_ops(siglen=32)).
func opclassShape(opclass string) string {
	i := strings.IndexByte(opclass, '(')
	if i < 0 {
		return opclass
	}
	return opclass[:i] + quotedNumber.ReplaceAllString(opclass[i:], "$1")
}

// orderSuffix renders a key's ordering, reversed when invert is set: ""
// for ASC NULLS LAST, otherwise " desc" and/or the non-default NULLS.
func orderSuffix(d keyDecor, invert bool) string {
	desc := d.order == "desc"
	nullsFirst := d.nulls == "first" || (d.nulls == "" && desc)
	if invert {
		desc, nullsFirst = !desc, !nullsFirst
	}
	s := ""
	if desc {
		s = " desc"
	}
	switch {
	case nullsFirst && !desc:
		s += " nulls first"
	case !nullsFirst && desc:
		s += " nulls last"
	}
	return s
}

// withoutDefaults is a plain column key's decoration with the collation
// and operator class dropped when they are the ones PostgreSQL applies
// without them (and pg_get_indexdef omits). Expression keys keep theirs:
// their type and collation are not known here.
func withoutDefaults(k indexKey, method string, defaults map[string]KeyDefault) keyDecor {
	d := k.decor
	def, ok := defaults[k.column]
	if k.column == "" || !ok {
		return d
	}
	if d.collation != "" && slices.Contains(def.Collation, d.collation) {
		d.collation = ""
	}
	if d.opclass != "" && slices.Contains(def.Opclass[method], d.opclass) {
		d.opclass = ""
	}
	return d
}
