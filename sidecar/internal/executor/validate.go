package executor

import (
	"fmt"
	"strings"
)

// ErrDisallowedSQL is returned when SQL fails the whitelist check.
var ErrDisallowedSQL = fmt.Errorf("disallowed SQL statement")

// allowedPrefixes lists the SQL statement types the executor
// is permitted to run. Each prefix is checked against the
// uppercased, trimmed SQL statement.
var allowedPrefixes = []string{
	"CREATE INDEX",
	"CREATE UNIQUE INDEX",
	"DROP INDEX",
	"REINDEX",
	"VACUUM",
	"ANALYZE",
	"ALTER TABLE",
	"ALTER SYSTEM SET",
	"ALTER SYSTEM RESET",
	"ALTER DATABASE",
	"SET ",
	"RESET ",
	"SELECT ",
	"INSERT INTO HINT_PLAN.HINTS",
	"DELETE FROM HINT_PLAN.HINTS",
}

// safeAlterSystemParams is the whitelist of GUC parameters that
// ALTER SYSTEM SET/RESET may target. Any parameter not in this
// list is rejected to prevent dangerous runtime changes.
var safeAlterSystemParams = map[string]bool{
	"work_mem":                             true,
	"maintenance_work_mem":                 true,
	"effective_cache_size":                 true,
	"shared_buffers":                       true,
	"max_wal_size":                         true,
	"min_wal_size":                         true,
	"checkpoint_completion_target":         true,
	"checkpoint_timeout":                   true,
	"random_page_cost":                     true,
	"effective_io_concurrency":             true,
	"max_parallel_workers_per_gather":      true,
	"max_parallel_workers":                 true,
	"max_parallel_maintenance_workers":     true,
	"autovacuum_vacuum_cost_delay":         true,
	"autovacuum_vacuum_cost_limit":         true,
	"autovacuum_naptime":                   true,
	"autovacuum_max_workers":               true,
	"autovacuum_vacuum_threshold":          true,
	"autovacuum_vacuum_scale_factor":       true,
	"autovacuum_analyze_threshold":         true,
	"autovacuum_analyze_scale_factor":      true,
	"wal_buffers":                          true,
	"default_statistics_target":            true,
	"huge_pages":                           true,
	"temp_buffers":                         true,
	"statement_timeout":                    true,
	"lock_timeout":                         true,
	"idle_in_transaction_session_timeout":  true,
	"log_min_duration_statement":           true,
	"track_activity_query_size":            true,
	"jit":                                  true,
}

// allowedSelectPatterns restricts SELECT to specific safe
// function calls instead of allowing arbitrary queries.
var allowedSelectPatterns = []string{
	"SELECT PG_TERMINATE_BACKEND(",
	"SELECT PG_CANCEL_BACKEND(",
}

// safeAlterTableSubcmds restricts ALTER TABLE to safe
// sub-commands only (storage params, tablespace moves).
var safeAlterTableSubcmds = []string{
	"SET (",
	"RESET (",
	"SET TABLESPACE",
}

// ValidateExecutorSQL checks that sql is a single allowed
// statement. It rejects multi-statement strings and any
// statement type not in the whitelist.
func ValidateExecutorSQL(sql string) error {
	trimmed := strings.TrimSpace(sql)
	if trimmed == "" {
		return fmt.Errorf("%w: empty SQL", ErrDisallowedSQL)
	}

	if err := rejectMultiStatement(trimmed); err != nil {
		return err
	}

	upper := strings.ToUpper(trimmed)
	for _, prefix := range allowedPrefixes {
		if !strings.HasPrefix(upper, prefix) {
			continue
		}
		// Secondary checks for dangerous prefixes.
		if err := checkSecondary(upper, prefix); err != nil {
			return err
		}
		return nil
	}

	return fmt.Errorf(
		"%w: statement must start with one of the "+
			"allowed prefixes (CREATE INDEX, DROP INDEX, "+
			"REINDEX, VACUUM, ANALYZE, ALTER TABLE, "+
			"ALTER SYSTEM, ALTER DATABASE, SET, RESET, "+
			"SELECT, INSERT INTO hint_plan.hints, "+
			"DELETE FROM hint_plan.hints)",
		ErrDisallowedSQL,
	)
}

// checkSecondary enforces additional restrictions on prefixes
// that require deeper validation (ALTER SYSTEM, SELECT, ALTER TABLE).
func checkSecondary(upper, prefix string) error {
	switch prefix {
	case "ALTER SYSTEM SET", "ALTER SYSTEM RESET":
		return checkAlterSystemParam(upper)
	case "SELECT ":
		return checkSelectPattern(upper)
	case "ALTER TABLE":
		return checkAlterTableSubcmd(upper)
	}
	return nil
}

// checkAlterSystemParam extracts the GUC parameter from an
// ALTER SYSTEM statement and verifies it is whitelisted.
func checkAlterSystemParam(upper string) error {
	param := extractAlterSystemParam(upper)
	if param == "" {
		return fmt.Errorf(
			"%w: cannot parse parameter from ALTER SYSTEM",
			ErrDisallowedSQL,
		)
	}
	if !safeAlterSystemParams[strings.ToLower(param)] {
		return fmt.Errorf(
			"%w: ALTER SYSTEM parameter %q not in whitelist",
			ErrDisallowedSQL, param,
		)
	}
	return nil
}

// extractAlterSystemParam parses the parameter name from
// ALTER SYSTEM SET <param> = ... or ALTER SYSTEM RESET <param>.
func extractAlterSystemParam(upper string) string {
	rest := upper
	if strings.HasPrefix(rest, "ALTER SYSTEM SET ") {
		rest = strings.TrimPrefix(rest, "ALTER SYSTEM SET ")
	} else if strings.HasPrefix(rest, "ALTER SYSTEM RESET ") {
		rest = strings.TrimPrefix(rest, "ALTER SYSTEM RESET ")
	} else {
		return ""
	}
	rest = strings.TrimSpace(rest)
	// Parameter name is the first token (before '=' or whitespace or ';').
	fields := strings.FieldsFunc(rest, func(r rune) bool {
		return r == ' ' || r == '=' || r == '\t' || r == ';'
	})
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// checkSelectPattern verifies the SELECT matches one of the
// allowed function-call patterns.
func checkSelectPattern(upper string) error {
	for _, pat := range allowedSelectPatterns {
		if strings.HasPrefix(upper, pat) {
			return nil
		}
	}
	return fmt.Errorf(
		"%w: only pg_terminate_backend and pg_cancel_backend "+
			"SELECT statements are allowed",
		ErrDisallowedSQL,
	)
}

// checkAlterTableSubcmd verifies that the ALTER TABLE statement
// uses a safe sub-command (SET storage params, RESET, tablespace).
func checkAlterTableSubcmd(upper string) error {
	// Strip "ALTER TABLE <name>" to get the sub-command.
	// Format: ALTER TABLE [IF EXISTS] [schema.]name <subcmd>
	sub := stripAlterTablePrefix(upper)
	if sub == "" {
		return fmt.Errorf(
			"%w: cannot parse ALTER TABLE sub-command",
			ErrDisallowedSQL,
		)
	}
	for _, safe := range safeAlterTableSubcmds {
		if strings.HasPrefix(sub, safe) {
			return nil
		}
	}
	return fmt.Errorf(
		"%w: ALTER TABLE sub-command not allowed "+
			"(only SET/RESET storage params and SET TABLESPACE)",
		ErrDisallowedSQL,
	)
}

// stripAlterTablePrefix removes "ALTER TABLE [IF EXISTS] <name>"
// and returns the remaining sub-command portion, uppercased.
func stripAlterTablePrefix(upper string) string {
	rest := strings.TrimPrefix(upper, "ALTER TABLE ")
	rest = strings.TrimSpace(rest)
	if strings.HasPrefix(rest, "IF EXISTS ") {
		rest = strings.TrimPrefix(rest, "IF EXISTS ")
		rest = strings.TrimSpace(rest)
	}
	// Skip the table name (possibly schema-qualified and/or quoted).
	// Find the first space after the table name token(s).
	idx := findEndOfTableName(rest)
	if idx < 0 || idx >= len(rest) {
		return ""
	}
	return strings.TrimSpace(rest[idx:])
}

// findEndOfTableName returns the index past the table name in
// a string like `"public"."t" SET (...)` or `public.t SET (...)`.
func findEndOfTableName(s string) int {
	i := 0
	for i < len(s) {
		if s[i] == '"' {
			// Skip quoted identifier.
			i++ // opening quote
			for i < len(s) && s[i] != '"' {
				i++
			}
			if i < len(s) {
				i++ // closing quote
			}
		} else if s[i] == ' ' || s[i] == '\t' {
			return i
		} else {
			i++
		}
	}
	return -1
}

// rejectMultiStatement rejects SQL containing a semicolon
// followed by non-whitespace, indicating multiple statements.
func rejectMultiStatement(sql string) error {
	idx := strings.Index(sql, ";")
	if idx < 0 {
		return nil
	}
	rest := strings.TrimSpace(sql[idx+1:])
	if rest != "" {
		return fmt.Errorf(
			"%w: multi-statement SQL is not allowed",
			ErrDisallowedSQL,
		)
	}
	return nil
}
