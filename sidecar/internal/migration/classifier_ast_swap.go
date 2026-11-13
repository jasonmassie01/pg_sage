package migration

import (
	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// lockModeNames maps LOCK TABLE's mode (PostgreSQL's LOCKMODE numbering)
// to the mode's name.
var lockModeNames = map[int32]string{
	1: "ACCESS SHARE", 2: "ROW SHARE", 3: "ROW EXCLUSIVE",
	4: lockShareUpdateExclusive, 5: "SHARE", 6: lockShareRowExclusive,
	7: "EXCLUSIVE", 8: lockAccessExclusive,
}

// swapForm maps the DDL an online column swap runs (migration-09b) that
// is not ALTER TABLE: LOCK TABLE, CREATE FUNCTION and ALTER SEQUENCE. Its
// UPDATE backfill is DML, which this classifier leaves unrecognized.
func (m astMapper) swapForm(n *pg_query.Node) ([]DDLClassification, bool) {
	switch s := n.GetNode().(type) {
	case *pg_query.Node_LockStmt:
		return m.lockTable(s.LockStmt)
	case *pg_query.Node_CreateFunctionStmt:
		// A new function locks no table. OR REPLACE changes what live
		// triggers and defaults run, so it is not proven safe.
		f := s.CreateFunctionStmt
		return nil, !f.GetReplace() && !f.GetIsProcedure()
	case *pg_query.Node_AlterSeqStmt:
		return nil, sequenceOptionsSafe(s.AlterSeqStmt.GetOptions())
	}
	return nil, false
}

// lockTable names the first relation and reports the lock the statement
// asks for; a mode outside PostgreSQL's eight is not known.
func (m astMapper) lockTable(s *pg_query.LockStmt) ([]DDLClassification, bool) {
	mode, ok := lockModeNames[s.GetMode()]
	rels := s.GetRelations()
	if !ok || len(rels) == 0 {
		return nil, false
	}
	c := m.rule("ddl_lock_table", rels[0].GetRangeVar())
	c.LockLevel = mode
	return []DDLClassification{c}, true
}

// dropTrigger names the trigger's table: DROP TRIGGER t ON s.tab parses
// to the name list [s, tab, t].
func (m astMapper) dropTrigger(s *pg_query.DropStmt) []DDLClassification {
	c := m.rule("ddl_drop_trigger", nil)
	if objs := s.GetObjects(); len(objs) > 0 {
		names := stringItems(objs[0].GetList().GetItems())
		if len(names) >= 2 {
			c.TableName = names[len(names)-2]
		}
		if len(names) >= 3 {
			c.SchemaName = names[len(names)-3]
		}
	}
	return []DDLClassification{c}
}

// sequenceOptionsSafe: AS and OWNED BY change neither the values a live
// default hands out nor its position; RESTART, INCREMENT and the rest do.
func sequenceOptionsSafe(opts []*pg_query.Node) bool {
	if len(opts) == 0 {
		return false
	}
	for _, o := range opts {
		switch o.GetDefElem().GetDefname() {
		case "as", "owned_by":
		default:
			return false
		}
	}
	return true
}
