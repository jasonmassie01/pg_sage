package executor

import (
	"strings"
)

func actionTypeForProposalSQL(sql string) string {
	upper := strings.ToUpper(normalizeSQLText(sql))
	switch {
	case strings.HasPrefix(upper, "ANALYZE "):
		return "analyze_table"
	case strings.HasPrefix(upper, "CREATE INDEX CONCURRENTLY ") ||
		strings.HasPrefix(upper, "CREATE UNIQUE INDEX CONCURRENTLY "):
		return "create_index_concurrently"
	case strings.HasPrefix(upper, "DROP INDEX CONCURRENTLY "):
		return "drop_unused_index"
	case isConcurrentReindexSQL(upper):
		return "reindex_concurrently"
	case (strings.HasPrefix(upper, "VACUUM ") || upper == "VACUUM") &&
		!isVacuumFullSQL(upper):
		return "vacuum_table"
	case strings.Contains(upper, "PG_CANCEL_BACKEND"):
		return "cancel_backend"
	case strings.Contains(upper, "PG_CANCEL_BACKEND"):
		return "cancel_backend"
	case strings.Contains(upper, "PG_TERMINATE_BACKEND"):
		return "terminate_backend"
	case strings.HasPrefix(upper, "ALTER SYSTEM SET ") ||
		strings.HasPrefix(upper, "ALTER SYSTEM RESET "):
		return "alter_system_guc"
	case strings.HasPrefix(upper, "ALTER DATABASE ") &&
		allowedAlterDatabaseParam(upper):
		return "alter_database_guc"
	case isSetTableAutovacuumSQL(upper):
		return "set_table_autovacuum"
	case strings.HasPrefix(upper, "ALTER TABLE "):
		return "alter_table"
	case strings.HasPrefix(upper, "INSERT INTO HINT_PLAN.HINTS"):
		return "apply_query_hint"
	case strings.HasPrefix(upper, "DELETE FROM HINT_PLAN.HINTS"):
		return "retire_query_hint"
	default:
		return ""
	}
}

func isConcurrentReindexSQL(upper string) bool {
	rest := strings.TrimSpace(strings.TrimPrefix(upper, "REINDEX "))
	for _, objectType := range []string{
		"INDEX ", "TABLE ", "SCHEMA ", "DATABASE ", "SYSTEM ",
	} {
		if strings.HasPrefix(rest, objectType) {
			rest = strings.TrimSpace(strings.TrimPrefix(rest, objectType))
			return strings.HasPrefix(rest, "CONCURRENTLY ")
		}
	}
	return false
}

func isSetTableAutovacuumSQL(upper string) bool {
	if !strings.HasPrefix(upper, "ALTER TABLE ") {
		return false
	}
	subcommand := stripAlterTablePrefix(upper)
	return strings.HasPrefix(subcommand, "SET (") &&
		strings.Contains(subcommand, "AUTOVACUUM_") &&
		requireSingleReloptionSubcmd(subcommand) == nil
}

func isVacuumFullSQL(upper string) bool {
	if strings.HasPrefix(upper, "VACUUM FULL ") || upper == "VACUUM FULL" {
		return true
	}
	if !strings.HasPrefix(upper, "VACUUM (") {
		return false
	}
	end := strings.IndexByte(upper, ')')
	if end < 0 {
		return true
	}
	options := strings.NewReplacer("(", " ", ")", " ", ",", " ").
		Replace(upper[:end+1])
	for _, option := range strings.Fields(options) {
		if option == "FULL" {
			return true
		}
	}
	return false
}
