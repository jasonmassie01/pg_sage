//go:build cgo

package sqlast

import (
	"sort"

	pg_query "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// References parses exactly one statement and lists the relations and
// columns it reads and writes, with the table locks it implies.
func References(sql string) (Refs, error) {
	_, stmt, err := parseOne(sql)
	if err != nil {
		return Refs{}, err
	}
	c := newRefCollector()
	kind := StmtOther
	switch {
	case stmt.GetSelectStmt() != nil:
		kind = StmtSelect
		c.analyzeSelect(stmt.GetSelectStmt(), nil)
	case stmt.GetInsertStmt() != nil:
		kind = StmtInsert
		c.setTarget(stmt.GetInsertStmt().GetRelation())
		c.analyzeInsert(stmt.GetInsertStmt(), nil)
	case stmt.GetUpdateStmt() != nil:
		kind = StmtUpdate
		c.setTarget(stmt.GetUpdateStmt().GetRelation())
		c.analyzeUpdate(stmt.GetUpdateStmt(), nil)
	case stmt.GetDeleteStmt() != nil:
		kind = StmtDelete
		c.setTarget(stmt.GetDeleteStmt().GetRelation())
		c.analyzeDelete(stmt.GetDeleteStmt(), nil)
	case stmt.GetMergeStmt() != nil:
		kind = StmtMerge
		c.setTarget(stmt.GetMergeStmt().GetRelation())
		c.analyzeMerge(stmt.GetMergeStmt(), nil)
	default:
		c.collectUtilityRelations(stmt)
	}
	return c.result(kind), nil
}

// refCollector accumulates what a statement touches across query levels.
type refCollector struct {
	target     *QualifiedName
	relations  map[QualifiedName]bool
	locks      map[QualifiedName]LockMode
	columns    map[ColumnPair]bool
	star       map[QualifiedName]bool
	ambiguous  map[string]map[QualifiedName]bool
	unresolved map[string]bool
	ctes       map[string]bool
	modifies   bool
}

func newRefCollector() *refCollector {
	return &refCollector{relations: map[QualifiedName]bool{},
		locks: map[QualifiedName]LockMode{}, columns: map[ColumnPair]bool{},
		star: map[QualifiedName]bool{}, ambiguous: map[string]map[QualifiedName]bool{},
		unresolved: map[string]bool{}, ctes: map[string]bool{}}
}

var lockStrength = map[LockMode]int{
	LockNone: 0, LockAccessShare: 1, LockRowShare: 2, LockRowExclusive: 3,
}

// lockRelation records a relation and raises its lock to at least mode.
func (c *refCollector) lockRelation(name QualifiedName, mode LockMode) {
	c.relations[name] = true
	if lockStrength[mode] > lockStrength[c.locks[name]] {
		c.locks[name] = mode
	}
}

func (c *refCollector) setTarget(rv *pg_query.RangeVar) {
	name := rangeVarName(rv)
	c.target = &name
}

func (c *refCollector) addColumns(pairs []ColumnPair) {
	for _, pair := range pairs {
		c.columns[pair] = true
	}
}

func (c *refCollector) addAmbiguous(column string, candidates []*rangeEntry) {
	set := c.ambiguous[column]
	if set == nil {
		set = map[QualifiedName]bool{}
		c.ambiguous[column] = set
	}
	for _, entry := range candidates {
		set[entry.displayName()] = true
	}
}

// collectUtilityRelations lists every relation a utility statement names;
// it implies no DML lock.
func (c *refCollector) collectUtilityRelations(stmt *pg_query.Node) {
	visitTree(stmt.ProtoReflect(), func(msg protoreflect.Message) bool {
		if rv, ok := msg.Interface().(*pg_query.RangeVar); ok {
			c.relations[rangeVarName(rv)] = true
		}
		return true
	})
}

func (c *refCollector) result(kind StatementKind) Refs {
	out := Refs{Kind: kind, Target: c.target, ModifiesData: c.modifies}
	out.Relations = sortedNames(c.relations)
	for _, name := range out.Relations {
		mode, ok := c.locks[name]
		if !ok {
			continue
		}
		out.Locks = append(out.Locks, RelationLock{Relation: name, Mode: mode})
		if lockStrength[mode] > lockStrength[out.LockMode] {
			out.LockMode = mode
		}
	}
	for pair := range c.columns {
		out.Columns = append(out.Columns, pair)
	}
	sort.Slice(out.Columns, func(i, j int) bool {
		a, b := out.Columns[i], out.Columns[j]
		if a.Relation != b.Relation {
			return lessName(a.Relation, b.Relation)
		}
		return a.Column < b.Column
	})
	out.Star = sortedNames(c.star)
	for column, set := range c.ambiguous {
		out.Ambiguous = append(out.Ambiguous,
			AmbiguousColumn{Column: column, Candidates: sortedNames(set)})
	}
	sort.Slice(out.Ambiguous, func(i, j int) bool {
		return out.Ambiguous[i].Column < out.Ambiguous[j].Column
	})
	out.Unresolved = sortedKeys(c.unresolved)
	out.CTENames = sortedKeys(c.ctes)
	return out
}

func lessName(a, b QualifiedName) bool {
	if a.Schema != b.Schema {
		return a.Schema < b.Schema
	}
	return a.Name < b.Name
}

func sortedNames(set map[QualifiedName]bool) []QualifiedName {
	var out []QualifiedName
	for name := range set {
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool { return lessName(out[i], out[j]) })
	return out
}

func sortedKeys(set map[string]bool) []string {
	var out []string
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// visitTree walks a parse tree depth first; visit returning false skips
// the node's children (but not its siblings).
func visitTree(msg protoreflect.Message, visit func(protoreflect.Message) bool) {
	if !visit(msg) {
		return
	}
	msg.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		switch {
		case field.IsList() && field.Message() != nil:
			list := value.List()
			for i := 0; i < list.Len(); i++ {
				visitTree(list.Get(i).Message(), visit)
			}
		case field.Message() != nil && !field.IsMap():
			visitTree(value.Message(), visit)
		}
		return true
	})
}
