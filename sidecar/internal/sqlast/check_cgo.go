//go:build cgo

package sqlast

import (
	"fmt"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// Available reports whether parse-tree validation is compiled in.
func Available() bool { return true }

// Check parses sql and accepts exactly one statement of an executor
// statement kind whose structure matches that kind's rules.
func Check(sql string, rules Rules) error {
	result, err := pg_query.Parse(sql)
	if err != nil {
		return reject("parse error: %v", err)
	}
	switch len(result.GetStmts()) {
	case 0:
		return reject("no statement")
	case 1:
	default:
		return reject("exactly one statement is allowed, got %d", len(result.GetStmts()))
	}
	stmt := result.GetStmts()[0].GetStmt()
	if schema := protectedRelation(stmt, rules); schema != "" {
		return reject("references protected schema %q", schema)
	}
	return checkStatement(stmt, rules)
}

func reject(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRejected, fmt.Sprintf(format, args...))
}

func checkStatement(stmt *pg_query.Node, rules Rules) error {
	switch {
	case stmt.GetIndexStmt() != nil:
		return nil
	case stmt.GetCreateStatsStmt() != nil:
		return checkCreateStats(stmt.GetCreateStatsStmt(), rules)
	case stmt.GetDropStmt() != nil:
		return checkDrop(stmt.GetDropStmt(), rules)
	case stmt.GetReindexStmt() != nil:
		return checkReindex(stmt.GetReindexStmt())
	case stmt.GetVacuumStmt() != nil:
		return checkVacuum(stmt.GetVacuumStmt())
	case stmt.GetAlterTableStmt() != nil:
		if stmt.GetAlterTableStmt().GetRelation().GetSchemaname() == "" {
			return reject("ALTER TABLE target must be schema-qualified")
		}
		return checkAlterTable(stmt.GetAlterTableStmt(), rules)
	case stmt.GetAlterSystemStmt() != nil:
		return checkSetting("ALTER SYSTEM",
			stmt.GetAlterSystemStmt().GetSetstmt(), rules.SystemParam)
	case stmt.GetAlterDatabaseSetStmt() != nil:
		return checkSetting("ALTER DATABASE",
			stmt.GetAlterDatabaseSetStmt().GetSetstmt(), rules.DatabaseParam)
	case stmt.GetSelectStmt() != nil:
		return checkBackendSignal(stmt.GetSelectStmt())
	case stmt.GetInsertStmt() != nil:
		return checkHintTable("INSERT", stmt.GetInsertStmt().GetRelation(), true)
	case stmt.GetDeleteStmt() != nil:
		del := stmt.GetDeleteStmt()
		return checkHintTable("DELETE", del.GetRelation(), del.GetWhereClause() != nil)
	default:
		return reject("statement kind is not allowed")
	}
}

func checkDrop(drop *pg_query.DropStmt, rules Rules) error {
	if drop.GetRemoveType() == pg_query.ObjectType_OBJECT_STATISTIC_EXT {
		return checkDropStats(drop, rules)
	}
	if drop.GetRemoveType() != pg_query.ObjectType_OBJECT_INDEX {
		return reject("DROP may remove only indexes or pg_sage statistics")
	}
	if len(drop.GetObjects()) != 1 {
		return reject("DROP INDEX must name exactly one index")
	}
	if drop.GetBehavior() == pg_query.DropBehavior_DROP_CASCADE {
		return reject("DROP INDEX ... CASCADE is not allowed")
	}
	// DROP names its object as a qualified-name list, not a RangeVar. An
	// unqualified name resolves through the session search_path at run
	// time ("$user" may be sage; pg_catalog is always searched), which no
	// static check can see, so it is refused. In catalog.schema.name the
	// schema is the second-to-last part.
	name := drop.GetObjects()[0].GetList().GetItems()
	if len(name) < 2 {
		return reject("DROP INDEX target must be schema-qualified")
	}
	schema := name[len(name)-2].GetString_().GetSval()
	if rules.ProtectedSchema != nil && rules.ProtectedSchema(strings.ToLower(schema)) {
		return reject("references protected schema %q", schema)
	}
	return nil
}

func checkReindex(reindex *pg_query.ReindexStmt) error {
	switch reindex.GetKind() {
	case pg_query.ReindexObjectType_REINDEX_OBJECT_INDEX,
		pg_query.ReindexObjectType_REINDEX_OBJECT_TABLE:
		return nil
	default:
		return reject("REINDEX may target only one index or table")
	}
}

func checkVacuum(vacuum *pg_query.VacuumStmt) error {
	for _, option := range vacuum.GetOptions() {
		if strings.EqualFold(option.GetDefElem().GetDefname(), "full") {
			return reject("VACUUM FULL is not allowed")
		}
	}
	return nil
}

func checkSetting(kind string, set *pg_query.VariableSetStmt, allowed func(string) bool) error {
	if set == nil {
		return reject("%s form is not allowed", kind)
	}
	name := strings.ToLower(set.GetName())
	if allowed == nil || !allowed(name) {
		return reject("%s parameter %q is not allowed", kind, name)
	}
	return nil
}

func checkHintTable(kind string, relation *pg_query.RangeVar, filtered bool) error {
	if relation.GetSchemaname() != "hint_plan" || relation.GetRelname() != "hints" {
		return reject("%s may target only hint_plan.hints", kind)
	}
	if !filtered {
		return reject("%s FROM hint_plan.hints needs a WHERE clause", kind)
	}
	return nil
}
