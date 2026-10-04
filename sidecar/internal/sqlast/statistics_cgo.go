//go:build cgo

package sqlast

import (
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// statisticsKinds are the kinds the statistics verifier judges.
var statisticsKinds = map[string]bool{"ndistinct": true, "dependencies": true, "mcv": true}

// checkCreateStats accepts the pg_sage CREATE STATISTICS form: one
// pg_sage-named object in the schema of the one schema-qualified table it
// reads, on plain columns, of the supported kinds. The protected-schema
// walk already saw the table; the object's own schema is checked here.
func checkCreateStats(stmt *pg_query.CreateStatsStmt, rules Rules) error {
	schema, name, err := statisticsName(stmt.GetDefnames(), "CREATE STATISTICS", rules)
	if err != nil {
		return err
	}
	if len(stmt.GetRelations()) != 1 || stmt.GetRelations()[0].GetRangeVar() == nil {
		return reject("CREATE STATISTICS must read exactly one table")
	}
	table := stmt.GetRelations()[0].GetRangeVar()
	if table.GetSchemaname() == "" {
		return reject("CREATE STATISTICS table must be schema-qualified")
	}
	if table.GetSchemaname() != schema {
		return reject("statistics %s.%s must live in the table's schema %s", schema, name,
			table.GetSchemaname())
	}
	for _, kind := range stmt.GetStatTypes() {
		if !statisticsKinds[kind.GetString_().GetSval()] {
			return reject("statistics kind %q is not allowed", kind.GetString_().GetSval())
		}
	}
	for _, expr := range stmt.GetExprs() {
		elem := expr.GetStatsElem()
		// An expression element has no name (and a column has no expression).
		if elem == nil || elem.GetName() == "" {
			return reject("CREATE STATISTICS may cover plain columns only")
		}
	}
	return nil
}

// checkDropStats accepts DROP STATISTICS of one pg_sage-named object,
// never CASCADE.
func checkDropStats(drop *pg_query.DropStmt, rules Rules) error {
	if len(drop.GetObjects()) != 1 {
		return reject("DROP STATISTICS must name exactly one statistics object")
	}
	if drop.GetBehavior() == pg_query.DropBehavior_DROP_CASCADE {
		return reject("DROP STATISTICS ... CASCADE is not allowed")
	}
	_, _, err := statisticsName(drop.GetObjects()[0].GetList().GetItems(), "DROP STATISTICS",
		rules)
	return err
}

// statisticsName is a statistics object's schema and name, which must be
// exactly schema.name, outside the protected schemas, and pg_sage's own.
func statisticsName(parts []*pg_query.Node, kind string, rules Rules) (string, string, error) {
	if len(parts) != 2 {
		return "", "", reject("%s object must be schema-qualified", kind)
	}
	schema, name := parts[0].GetString_().GetSval(), parts[1].GetString_().GetSval()
	if rules.ProtectedSchema != nil && rules.ProtectedSchema(strings.ToLower(schema)) {
		return "", "", reject("references protected schema %q", schema)
	}
	if rules.StatisticsName == nil || !rules.StatisticsName(name) {
		return "", "", reject("%s may name only pg_sage statistics, not %q", kind, name)
	}
	return schema, name, nil
}
