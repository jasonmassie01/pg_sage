//go:build cgo

package sqlast

import (
	"fmt"

	pg_query "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ReadShape returns the read statement that finds the rows a write would
// touch, so plan-only EXPLAIN can estimate it without the write:
// UPDATE/DELETE becomes SELECT 1 FROM rel WHERE cond, INSERT ... SELECT
// (or VALUES) becomes its SELECT, and a SELECT is returned without locking
// clauses. Statement CTEs are kept. UPDATE ... FROM, DELETE ... USING,
// WHERE CURRENT OF, MERGE, data-modifying CTEs, SELECT INTO and every other
// statement return ErrUnsupported.
func ReadShape(sql string) (string, error) {
	_, stmt, err := parseOne(sql)
	if err != nil {
		return "", err
	}
	sel, err := readShapeOf(stmt)
	if err != nil {
		return "", err
	}
	if writes, kind := containsWrite(sel); writes {
		return "", unsupported("a data-modifying %s in a CTE has no read shape", kind)
	}
	stripLocking(sel)
	out, err := deparseSelect(sel)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	return out, nil
}

func unsupported(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsupported, fmt.Sprintf(format, args...))
}

func readShapeOf(stmt *pg_query.Node) (*pg_query.SelectStmt, error) {
	switch {
	case stmt.GetUpdateStmt() != nil:
		upd := stmt.GetUpdateStmt()
		if len(upd.GetFromClause()) > 0 {
			return nil, unsupported("UPDATE ... FROM")
		}
		if upd.GetWhereClause().GetCurrentOfExpr() != nil {
			return nil, unsupported("UPDATE ... WHERE CURRENT OF")
		}
		return rowFinder(upd.GetRelation(), upd.GetWhereClause(), upd.GetWithClause()), nil
	case stmt.GetDeleteStmt() != nil:
		del := stmt.GetDeleteStmt()
		if len(del.GetUsingClause()) > 0 {
			return nil, unsupported("DELETE ... USING")
		}
		if del.GetWhereClause().GetCurrentOfExpr() != nil {
			return nil, unsupported("DELETE ... WHERE CURRENT OF")
		}
		return rowFinder(del.GetRelation(), del.GetWhereClause(), del.GetWithClause()), nil
	case stmt.GetInsertStmt() != nil:
		return insertSource(stmt.GetInsertStmt())
	case stmt.GetSelectStmt() != nil:
		if stmt.GetSelectStmt().GetIntoClause() != nil {
			return nil, unsupported("SELECT INTO")
		}
		return stmt.GetSelectStmt(), nil
	}
	return nil, unsupported("%s", nodeName(stmt))
}

// rowFinder builds SELECT 1 FROM rel WHERE where, with the statement's CTEs.
func rowFinder(rel *pg_query.RangeVar, where *pg_query.Node,
	with *pg_query.WithClause) *pg_query.SelectStmt {
	one := &pg_query.ResTarget{Val: pg_query.MakeAConstIntNode(1, -1)}
	return &pg_query.SelectStmt{
		TargetList:  []*pg_query.Node{{Node: &pg_query.Node_ResTarget{ResTarget: one}}},
		FromClause:  []*pg_query.Node{{Node: &pg_query.Node_RangeVar{RangeVar: rel}}},
		WhereClause: where,
		WithClause:  with,
	}
}

// insertSource is the SELECT or VALUES an INSERT inserts, carrying the
// INSERT's own CTEs.
func insertSource(ins *pg_query.InsertStmt) (*pg_query.SelectStmt, error) {
	sel := ins.GetSelectStmt().GetSelectStmt()
	if sel == nil {
		return nil, unsupported("INSERT ... DEFAULT VALUES")
	}
	if outer := ins.GetWithClause(); outer != nil {
		if inner := sel.GetWithClause(); inner != nil {
			outer.Ctes = append(outer.Ctes, inner.GetCtes()...)
			outer.Recursive = outer.GetRecursive() || inner.GetRecursive()
		}
		sel.WithClause = outer
	}
	return sel, nil
}

// containsWrite reports a nested INSERT, UPDATE, DELETE or MERGE.
func containsWrite(sel *pg_query.SelectStmt) (bool, string) {
	kind := ""
	walk(sel.ProtoReflect(), func(msg protoreflect.Message) bool {
		switch msg.Interface().(type) {
		case *pg_query.InsertStmt:
			kind = "INSERT"
		case *pg_query.UpdateStmt:
			kind = "UPDATE"
		case *pg_query.DeleteStmt:
			kind = "DELETE"
		case *pg_query.MergeStmt:
			kind = "MERGE"
		}
		return kind == ""
	})
	return kind != "", kind
}

// stripLocking drops FOR UPDATE/SHARE clauses at every query level: the
// plan of the row search is the same without them.
func stripLocking(sel *pg_query.SelectStmt) {
	walk(sel.ProtoReflect(), func(msg protoreflect.Message) bool {
		if s, ok := msg.Interface().(*pg_query.SelectStmt); ok {
			s.LockingClause = nil
		}
		return true
	})
}
