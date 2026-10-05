package optimizer

import "strings"

// coverShape is an index's shape for coverage: ordered key shapes and the
// columns it carries in INCLUDE.
type coverShape struct {
	method, where string
	unique        bool
	keys, include []string
}

func coverShapeOf(ddl string) (coverShape, bool) {
	spec, err := ParseIndexDDL(ddl)
	if err != nil {
		return coverShape{}, false
	}
	keys, include, ok := indexColumns(ddl)
	if !ok {
		return coverShape{}, false
	}
	return coverShape{method: spec.Method, where: canonicalShape(spec.Where),
		unique: spec.Unique, keys: keyShapes(keys), include: keyShapes(include)}, true
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
func CoveredBy(candidateDDL, existingDef string) bool {
	return covers(existingDef, candidateDDL, false)
}

// Subsumes reports whether the wider index serves every lookup of the
// narrower one (both a DDL or a pg_get_indexdef definition): CoveredBy's
// key, INCLUDE and uniqueness rules, with the wider index's predicate
// implied by the narrower one's (its AND-ed conjuncts are a subset). An
// OR predicate compares exactly. lifeos 1.10.0: (status, fact_type,
// quality_score) WHERE valid_to IS NULL AND deleted_at IS NULL subsumes the
// same keys WHERE ... AND quality_score IS NOT NULL.
func Subsumes(widerDef, narrowerDef string) bool {
	return covers(widerDef, narrowerDef, true)
}

// LeadingKey is the first key of an index DDL, lower-cased.
func LeadingKey(ddl string) (string, bool) {
	s, ok := coverShapeOf(ddl)
	if !ok || len(s.keys) == 0 {
		return "", false
	}
	return strings.ToLower(s.keys[0]), true
}

// covers reports whether the index have serves every lookup of want; with
// implied, have's predicate may be weaker than want's.
func covers(haveDef, wantDef string, implied bool) bool {
	want, ok := coverShapeOf(wantDef)
	if !ok || len(want.keys) == 0 {
		return false
	}
	have, ok := coverShapeOf(haveDef)
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
	served := make(map[string]bool, len(have.keys)+len(have.include))
	for _, c := range append(append([]string(nil), have.keys...), have.include...) {
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
