package migration

import (
	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// astStatement classifies one sanitized, collapsed statement from its
// parse tree (migration-04). A statement that does not parse, or that
// parses to a form the tree mapping has no entry for, is unrecognized; it
// still gets every regex rule match, so the deterministic lock floor is
// never lower than the regex path's.
func astStatement(stmt string, version int) ([]DDLClassification, bool) {
	major := serverMajor(version)
	tree, err := parseStatement(stmt)
	if err != nil || len(tree.GetStmts()) != 1 {
		if found, ok := notNullConstraintText(stmt, major); ok {
			return found, true
		}
		return regexRules.classifyStatement(stmt), false
	}
	m := astMapper{stmt: stmt, major: major}
	found, known := m.statement(tree.GetStmts()[0].GetStmt())
	if !known {
		return mergeRules(found, regexRules.classifyStatement(stmt)), false
	}
	return found, true
}

// parserGrammar is the grammar of the bundled parser (pg_query_go v6,
// libpg_query): a statement only a newer server accepts, such as a PG 18
// VIRTUAL generated column, is refused as a syntax error, so the note
// names the grammar.
const parserGrammar = "PostgreSQL 17"

// parseRefusal is the SQL parser's error for one sanitized statement, the
// one astStatement saw ("" when it parses, or when the build has no
// parser: the report's degraded note says so instead). The statement is
// redacted, so the message holds no literal.
func parseRefusal(stmt string) string {
	if !parserAvailable {
		return ""
	}
	if _, err := parseStatement(stmt); err != nil {
		return err.Error()
	}
	return ""
}

// serverMajor maps server_version_num (170004) or a bare major (17) to
// the major; 0 means unknown.
func serverMajor(version int) int {
	switch {
	case version >= 10000:
		return version / 10000
	case version > 0 && version < 100:
		return version
	}
	return 0
}

// atLeast reports whether the server is at least major; an unknown
// server counts as new enough, so a hazard of the newer major is flagged.
func (m astMapper) atLeast(major int) bool { return m.major == 0 || m.major >= major }

// astMapper maps one parsed statement to rule classifications.
type astMapper struct {
	stmt  string
	major int
}

// rule builds a classification of the mapper's statement.
func (m astMapper) rule(id string, rv *pg_query.RangeVar) DDLClassification {
	c := newClassification(catalogRule(id), m.stmt)
	if rv != nil {
		c.SchemaName, c.TableName = rv.GetSchemaname(), rv.GetRelname()
	}
	return c
}

// statement returns the rules a statement matches and whether it is a
// form the mapping knows. Statements outside the table are unrecognized.
func (m astMapper) statement(n *pg_query.Node) ([]DDLClassification, bool) {
	switch s := n.GetNode().(type) {
	case *pg_query.Node_IndexStmt:
		return m.createIndex(s.IndexStmt), true
	case *pg_query.Node_AlterTableStmt:
		return m.alterTable(s.AlterTableStmt)
	case *pg_query.Node_DropStmt:
		return m.drop(s.DropStmt)
	case *pg_query.Node_ReindexStmt:
		return m.reindex(s.ReindexStmt), true
	case *pg_query.Node_VacuumStmt:
		return m.vacuum(s.VacuumStmt), true
	case *pg_query.Node_ClusterStmt:
		return []DDLClassification{m.rule("ddl_cluster", s.ClusterStmt.GetRelation())}, true
	case *pg_query.Node_RefreshMatViewStmt:
		return m.refresh(s.RefreshMatViewStmt), true
	case *pg_query.Node_CreateTrigStmt:
		return []DDLClassification{
			m.rule("ddl_create_trigger", s.CreateTrigStmt.GetRelation())}, true
	case *pg_query.Node_RenameStmt:
		return m.rename(s.RenameStmt)
	case *pg_query.Node_CreateStmt:
		return nil, createTableKnown(s.CreateStmt)
	case *pg_query.Node_CreateSchemaStmt:
		return nil, len(s.CreateSchemaStmt.GetSchemaElts()) == 0
	case *pg_query.Node_LockStmt, *pg_query.Node_CreateFunctionStmt,
		*pg_query.Node_AlterSeqStmt:
		return m.swapForm(n)
	case *pg_query.Node_VariableSetStmt, *pg_query.Node_TransactionStmt,
		*pg_query.Node_ViewStmt, *pg_query.Node_CommentStmt, *pg_query.Node_CreateSeqStmt:
		return nil, true
	}
	return nil, false
}

func (m astMapper) createIndex(s *pg_query.IndexStmt) []DDLClassification {
	if s.GetConcurrent() {
		return nil
	}
	c := m.rule("ddl_index_not_concurrent", s.GetRelation())
	c.SafeAlternative = concurrentIndexSQL(m.stmt, c.SafeAlternative)
	return []DDLClassification{c}
}

// drop knows DROP TABLE, a plain DROP INDEX, DROP TRIGGER and DROP
// FUNCTION, each of which matches a rule. Other drops, including DROP
// INDEX CONCURRENTLY, take no table-wide lock pg_sage measures but still
// remove an object, so they are not proven safe.
func (m astMapper) drop(s *pg_query.DropStmt) ([]DDLClassification, bool) {
	switch s.GetRemoveType() {
	case pg_query.ObjectType_OBJECT_TABLE:
		c := m.rule("ddl_drop_table", nil)
		c.SchemaName, c.TableName = dropTarget(s)
		return []DDLClassification{c}, true
	case pg_query.ObjectType_OBJECT_INDEX:
		if !s.GetConcurrent() {
			return []DDLClassification{m.rule("ddl_drop_index_not_concurrent", nil)}, true
		}
	case pg_query.ObjectType_OBJECT_TRIGGER:
		return m.dropTrigger(s), true
	case pg_query.ObjectType_OBJECT_FUNCTION:
		return []DDLClassification{m.rule("ddl_drop_function", nil)}, true
	}
	return nil, false
}

// dropTarget is the (schema, name) of a DROP statement's first object.
func dropTarget(s *pg_query.DropStmt) (string, string) {
	objs := s.GetObjects()
	if len(objs) == 0 {
		return "", ""
	}
	names := stringItems(objs[0].GetList().GetItems())
	switch len(names) {
	case 0:
		return "", ""
	case 1:
		return "", names[0]
	}
	return names[len(names)-2], names[len(names)-1]
}

func (m astMapper) reindex(s *pg_query.ReindexStmt) []DDLClassification {
	if defOption(s.GetParams(), "concurrently") {
		return nil
	}
	rv := s.GetRelation()
	if s.GetKind() != pg_query.ReindexObjectType_REINDEX_OBJECT_TABLE {
		rv = nil
	}
	return []DDLClassification{m.rule("ddl_reindex_not_concurrent", rv)}
}

// vacuum flags VACUUM FULL; ANALYZE and a plain VACUUM are safe forms.
func (m astMapper) vacuum(s *pg_query.VacuumStmt) []DDLClassification {
	if !s.GetIsVacuumcmd() || !defOption(s.GetOptions(), "full") {
		return nil
	}
	var rv *pg_query.RangeVar
	if rels := s.GetRels(); len(rels) > 0 {
		rv = rels[0].GetVacuumRelation().GetRelation()
	}
	return []DDLClassification{m.rule("ddl_vacuum_full", rv)}
}

func (m astMapper) refresh(s *pg_query.RefreshMatViewStmt) []DDLClassification {
	if s.GetConcurrent() {
		return nil
	}
	return []DDLClassification{m.rule("ddl_refresh_not_concurrent", s.GetRelation())}
}

// rename knows a relation rename (table, column, constraint, view,
// sequence, index). An index rename takes SHARE UPDATE EXCLUSIVE on PG
// 12+; the others take ACCESS EXCLUSIVE. Renames of other objects are
// not table DDL the catalog measures, so they are unrecognized.
func (m astMapper) rename(s *pg_query.RenameStmt) ([]DDLClassification, bool) {
	if s.GetRelation() == nil {
		return nil, false
	}
	c := m.rule("ddl_rename", s.GetRelation())
	switch s.GetRenameType() {
	case pg_query.ObjectType_OBJECT_INDEX:
		c.SchemaName, c.TableName = "", ""
		c.LockLevel = "SHARE UPDATE EXCLUSIVE"
	case pg_query.ObjectType_OBJECT_COLUMN:
		c.ColumnName = s.GetSubname()
	}
	return []DDLClassification{c}, true
}

// createTableKnown: a new, empty table locks nothing else unless it is a
// partition (ACCESS EXCLUSIVE on the parent), inherits (locks the
// parent) or references another table (SHARE ROW EXCLUSIVE on it).
func createTableKnown(s *pg_query.CreateStmt) bool {
	if s.GetPartbound() != nil || len(s.GetInhRelations()) > 0 {
		return false
	}
	for _, n := range s.GetConstraints() {
		if n.GetConstraint().GetContype() == pg_query.ConstrType_CONSTR_FOREIGN {
			return false
		}
	}
	for _, n := range s.GetTableElts() {
		for _, cn := range n.GetColumnDef().GetConstraints() {
			if cn.GetConstraint().GetContype() == pg_query.ConstrType_CONSTR_FOREIGN {
				return false
			}
		}
		if con := n.GetConstraint(); con.GetContype() == pg_query.ConstrType_CONSTR_FOREIGN {
			return false
		}
	}
	return true
}
