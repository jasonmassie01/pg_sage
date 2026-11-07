package optimizer

import "strings"

// coverShape is an index's shape for coverage: ordered key shapes (with
// collation, operator class and, for btree, ordering), the bare key
// columns and expressions, and the columns it carries in INCLUDE.
type coverShape struct {
	method, where        string
	unique               bool
	keys, bases, include []string
}

// coverShapeOf parses an index DDL or definition; defaults are the
// table's column defaults (nil: none known, so every explicit opclass and
// collation stays part of the key).
func coverShapeOf(ddl string, defaults map[string]KeyDefault) (coverShape, bool) {
	spec, err := ParseIndexDDL(ddl)
	if err != nil {
		return coverShape{}, false
	}
	keys, include, ok := indexColumns(ddl)
	if !ok {
		return coverShape{}, false
	}
	return coverShape{method: spec.Method, where: canonicalShape(spec.Where),
		unique: spec.Unique, keys: shapeKeys(keys, spec.Method, defaults),
		bases: keyBases(keys), include: keyBases(include)}, true
}

// CoveredBy reports whether the existing index (a pg_get_indexdef
// definition) already serves every lookup the candidate DDL would: same
// method and predicate, the candidate's keys are a leading prefix of the
// existing keys (equal keys for non-btree methods, whose prefixes are not
// searchable the same way), and every INCLUDE column of the candidate is
// carried by the existing index (lifeos 1.8.3, action 6400: (node_type,
// name) proposed beside (node_type, name) INCLUDE (id)). A UNIQUE
// candidate is a constraint: only a unique index on exactly its keys
// covers it. It is the one coverage rule: the optimizer applies it when it
// proposes, the executor before it queues or runs any index create.
//
// Keys match only when expression, operator class and collation are all
// equal (indexes-01: text_pattern_ops is not a plain btree, and
// vector_cosine_ops is not vector_l2_ops). For btree the compared prefix
// must have the same ordering or be its uniform reversal, NULLS included.
// Without the catalog an explicitly named default opclass or collation
// does not match an omitted one, which errs toward "not covered".
func CoveredBy(candidateDDL, existingDef string) bool {
	return covers(existingDef, candidateDDL, false, nil)
}

// Subsumes reports whether the wider index serves every lookup of the
// narrower one (both a DDL or a pg_get_indexdef definition): CoveredBy's
// key, INCLUDE and uniqueness rules, with the wider index's predicate
// implied by the narrower one's (its AND-ed conjuncts are a subset). An
// OR predicate compares exactly. lifeos 1.10.0: (status, fact_type,
// quality_score) WHERE valid_to IS NULL AND deleted_at IS NULL subsumes the
// same keys WHERE ... AND quality_score IS NOT NULL.
func Subsumes(widerDef, narrowerDef string) bool {
	return covers(widerDef, narrowerDef, true, nil)
}

// LeadingKey is the first key of an index DDL, lower-cased, without its
// collation, operator class or ordering.
func LeadingKey(ddl string) (string, bool) {
	s, ok := coverShapeOf(ddl, nil)
	if !ok || len(s.bases) == 0 {
		return "", false
	}
	return strings.ToLower(s.bases[0]), true
}

// covers reports whether the index have serves every lookup of want; with
// implied, have's predicate may be weaker than want's. defaults normalize
// both sides' keys (nil: none known).
func covers(haveDef, wantDef string, implied bool, defaults map[string]KeyDefault) bool {
	want, ok := coverShapeOf(wantDef, defaults)
	if !ok || len(want.keys) == 0 {
		return false
	}
	have, ok := coverShapeOf(haveDef, defaults)
	if !ok || have.method != want.method || !predicateServes(have.where, want.where,
		implied) || len(want.keys) > len(have.keys) ||
		(want.method != "btree" && len(want.keys) != len(have.keys)) ||
		(want.unique && (!have.unique || len(want.keys) != len(have.keys))) {
		return false
	}
	for i, key := range want.keys {
		if have.keys[i] != key {
			return false
		}
	}
	served := make(map[string]bool, len(have.bases)+len(have.include))
	for _, c := range append(append([]string(nil), have.bases...), have.include...) {
		served[c] = true
	}
	for _, c := range want.include {
		if !served[c] {
			return false
		}
	}
	return true
}

// predicateServes reports whether an index with predicate have can serve
// lookups that satisfy want: equal predicates, or (implied) have's AND-ed
// conjuncts all among want's. No predicate serves everything.
func predicateServes(have, want string, implied bool) bool {
	if have == want {
		return true
	}
	if !implied || hasOr(have) || hasOr(want) {
		return false
	}
	if have == "" {
		return true
	}
	wanted := map[string]bool{}
	for _, c := range andTerms(want) {
		wanted[c] = true
	}
	for _, c := range andTerms(have) {
		if !wanted[c] {
			return false
		}
	}
	return true
}

func hasOr(pred string) bool {
	for _, w := range strings.Fields(pred) {
		if strings.EqualFold(w, "or") {
			return true
		}
	}
	return false
}

// andTerms splits a canonical predicate on AND, lower-cased.
func andTerms(pred string) []string {
	var out, cur []string
	for _, w := range strings.Fields(pred) {
		if strings.EqualFold(w, "and") {
			out, cur = append(out, strings.ToLower(strings.Join(cur, " "))), nil
			continue
		}
		cur = append(cur, w)
	}
	if len(cur) > 0 {
		out = append(out, strings.ToLower(strings.Join(cur, " ")))
	}
	return out
}
