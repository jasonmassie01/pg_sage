package advisor

import (
	"maps"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/managedparam"
	"github.com/pg-sage/sidecar/internal/pgconf"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// databaseScopeReason is why a database-scope setting waits for an operator.
func databaseScopeReason(name string) string {
	return name + " is changed only for this database (ALTER DATABASE), and only " +
		"with an operator's approval: it ends sessions, which pg_sage cannot verify " +
		"is harmless"
}

// scopeToDatabase keeps a database-scope setting
// (pgconf.DatabaseScopeApprovalOnly) at database scope (advisors-07). It runs
// after TransformForCloud, which already routed a managed service's change
// by the setting's context: an ALTER SYSTEM left over (self-managed) becomes
// ALTER DATABASE on dbName, quoted, and approval-required; without a
// database name it becomes advice, since the executor refuses the setting
// under ALTER SYSTEM. A provider parameter change would be instance-wide, so
// such an intent is dropped and the finding stays advice.
func scopeToDatabase(findings []analyzer.Finding, dbName string) []analyzer.Finding {
	for i, f := range findings {
		if in, ok := managedparam.IntentFromDetail(f.Detail); ok &&
			pgconf.DatabaseScopeApprovalOnly(in.Parameter) {
			f.Detail = maps.Clone(f.Detail)
			delete(f.Detail, managedparam.DetailKey)
			findings[i] = advisoryOnly(f, in.Parameter+" is changed only per database, "+
				"never instance-wide through the provider")
			continue
		}
		stmt, ok := pgconf.ParseAlterSystem(f.RecommendedSQL)
		if !ok || !pgconf.DatabaseScopeApprovalOnly(stmt.Name) {
			continue
		}
		if strings.TrimSpace(dbName) == "" {
			findings[i] = advisoryOnly(f, stmt.Name+" is changed only per database, "+
				"and no database name is known to scope it to")
			continue
		}
		findings[i] = databaseScopedChange(f, stmt, dbName)
	}
	return findings
}

// databaseScopedChange is the ALTER DATABASE form of stmt. The rollback is a
// hint; the executor captures the prior database value at apply time and
// rolls back to it.
func databaseScopedChange(f analyzer.Finding, stmt pgconf.SystemStmt,
	dbName string) analyzer.Finding {
	db := "ALTER DATABASE " + sanitize.QuoteIdentifier(dbName)
	f.RollbackSQL = ""
	if stmt.Reset {
		f.RecommendedSQL = db + " RESET " + stmt.Name
	} else {
		f.RecommendedSQL = db + " SET " + stmt.Name + " = " + sanitize.QuoteLiteral(stmt.Value)
		f.RollbackSQL = db + " RESET " + stmt.Name
	}
	f.ActionRisk = "moderate"
	return withApprovalRequired(f, databaseScopeReason(stmt.Name))
}
