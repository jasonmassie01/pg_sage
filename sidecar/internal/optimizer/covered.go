package optimizer

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
	want, ok := coverShapeOf(candidateDDL)
	if !ok || len(want.keys) == 0 {
		return false
	}
	have, ok := coverShapeOf(existingDef)
	if !ok || have.method != want.method || have.where != want.where ||
		len(want.keys) > len(have.keys) ||
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
