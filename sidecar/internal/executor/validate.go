package executor

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/pgconf"
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
	"SELECT ",
	"INSERT INTO HINT_PLAN.HINTS",
	"DELETE FROM HINT_PLAN.HINTS",
}

var backendSignalPattern = regexp.MustCompile(
	`(?i)^\s*SELECT\s+PG_(CANCEL|TERMINATE)_BACKEND\s*` +
		`\(\s*([0-9]+)\s*\)\s*;?\s*$`,
)

const migrationIdentifier = `(?:"(?:[^"]|"")*"|[A-Z_][A-Z0-9_$]*)`

var safeMigrationSubcommands = []*regexp.Regexp{
	regexp.MustCompile(`^ADD\s+CONSTRAINT\s+` + migrationIdentifier +
		`\s+CHECK\s*\(\s*` + migrationIdentifier + `\s+IS\s+NOT\s+NULL\s*\)` +
		`\s+NOT\s+VALID\s*;?$`),
	regexp.MustCompile(`^VALIDATE\s+CONSTRAINT\s+` + migrationIdentifier + `\s*;?$`),
	regexp.MustCompile(`^ALTER\s+COLUMN\s+` + migrationIdentifier +
		`\s+SET\s+NOT\s+NULL\s*;?$`),
	regexp.MustCompile(`^ADD\s+CONSTRAINT\s+` + migrationIdentifier +
		`\s+UNIQUE\s+USING\s+INDEX\s+` + migrationIdentifier + `\s*;?$`),
	regexp.MustCompile(`^DROP\s+CONSTRAINT\s+` + migrationIdentifier + `\s*;?$`),
}

// safeAlterTableSubcmds restricts ALTER TABLE to a single storage
// parameter sub-command. SET TABLESPACE (a full rewrite under ACCESS
// EXCLUSIVE) is not executor work.
var safeAlterTableSubcmds = []string{
	"SET (",
	"RESET (",
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
	if hasSQLComment(trimmed) {
		return fmt.Errorf("%w: SQL comments are not allowed", ErrDisallowedSQL)
	}

	normalized := normalizeSQLText(trimmed)
	upper := strings.ToUpper(normalized)
	for _, prefix := range allowedPrefixes {
		if !strings.HasPrefix(upper, prefix) {
			continue
		}
		// Secondary checks for dangerous prefixes.
		if err := checkSecondary(upper, prefix); err != nil {
			return err
		}
		if err := checkProtectedSchemaUsage(normalized, prefix); err != nil {
			return err
		}
		return checkParseTree(trimmed)
	}

	return fmt.Errorf(
		"%w: statement must start with one of the "+
			"allowed prefixes (CREATE INDEX, DROP INDEX, "+
			"REINDEX, VACUUM, ANALYZE, ALTER TABLE, "+
			"ALTER SYSTEM, ALTER DATABASE, "+
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
	case "ALTER DATABASE":
		if !allowedAlterDatabaseParam(upper) {
			return fmt.Errorf(
				"%w: ALTER DATABASE parameter is not in the GUC allowlist",
				ErrDisallowedSQL)
		}
		return nil
	case "SELECT ":
		return checkSelectPattern(upper)
	case "ALTER TABLE":
		return checkAlterTableSubcmd(upper)
	}
	return nil
}

func allowedAlterDatabaseParam(upper string) bool {
	rest, ok := alterDatabaseClause(upper)
	if !ok {
		return false
	}
	if strings.HasPrefix(rest, "SET ") {
		rest = strings.TrimPrefix(rest, "SET ")
	} else if strings.HasPrefix(rest, "RESET ") {
		rest = strings.TrimPrefix(rest, "RESET ")
	} else {
		return false
	}
	fields := strings.FieldsFunc(rest, func(r rune) bool {
		return r == ' ' || r == '=' || r == '\t' || r == ';'
	})
	return len(fields) > 0 && pgconf.ExecutableGUC(fields[0])
}

func alterDatabaseClause(upper string) (string, bool) {
	rest := strings.TrimSpace(strings.TrimPrefix(upper, "ALTER DATABASE "))
	if rest == "" || rest == upper {
		return "", false
	}
	if rest[0] != '"' {
		idx := strings.IndexAny(rest, " \t\r\n")
		if idx < 0 {
			return "", false
		}
		return strings.TrimSpace(rest[idx:]), true
	}
	for i := 1; i < len(rest); i++ {
		if rest[i] != '"' {
			continue
		}
		if i+1 < len(rest) && rest[i+1] == '"' {
			i++
			continue
		}
		return strings.TrimSpace(rest[i+1:]), true
	}
	return "", false
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
	if !pgconf.ExecutableGUC(param) {
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
	if _, _, ok := parseBackendSignal(upper); ok {
		return nil
	}
	return fmt.Errorf(
		"%w: only pg_terminate_backend and pg_cancel_backend "+
			"SELECT statements are allowed",
		ErrDisallowedSQL,
	)
}

func parseBackendSignal(sql string) (string, int, bool) {
	matches := backendSignalPattern.FindStringSubmatch(sql)
	if len(matches) != 3 {
		return "", 0, false
	}
	pid, err := strconv.Atoi(matches[2])
	if err != nil || pid <= 0 {
		return "", 0, false
	}
	return strings.ToLower(matches[1]), pid, true
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
			if err := requireSingleReloptionSubcmd(sub); err != nil {
				return err
			}
			return checkReloptionAllowlist(upper)
		}
	}
	for _, pattern := range safeMigrationSubcommands {
		if pattern.MatchString(sub) {
			return nil
		}
	}
	return fmt.Errorf(
		"%w: ALTER TABLE sub-command not allowed "+
			"(only safe storage or rehearsed migration forms)",
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
			for i < len(s) {
				if s[i] != '"' {
					i++
					continue
				}
				if i+1 < len(s) && s[i+1] == '"' {
					i += 2
					continue
				}
				i++ // closing quote
				break
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

func checkProtectedSchemaUsage(trimmed, prefix string) error {
	ident := statementTarget(trimmed, prefix)
	if ident == "" {
		return nil
	}
	schema := schemaFromIdentifier(ident)
	if isProtectedExecutorSchema(schema) {
		return fmt.Errorf(
			"%w: executor may not target protected schema %q",
			ErrDisallowedSQL, schema)
	}
	return nil
}

func containsFold(values []string, v string) bool {
	for _, value := range values {
		if strings.EqualFold(value, v) {
			return true
		}
	}
	return false
}

func schemaFromIdentifier(ident string) string {
	ident = strings.TrimSpace(ident)
	if ident == "" {
		return ""
	}
	dot := strings.LastIndex(ident, ".")
	if dot < 0 {
		return ""
	}
	return strings.Trim(ident[:dot], `"`)
}

func isProtectedExecutorSchema(schema string) bool {
	schema = strings.ToLower(strings.Trim(schema, `"`))
	switch schema {
	case "pg_catalog", "information_schema", "google_ml", "sage":
		return true
	}
	return strings.HasPrefix(schema, "_timescaledb_")
}

// checkReloptionAllowlist applies pgconf's executor reloption rule to an
// ALTER TABLE ... SET/RESET (G-P0-1): only allowlisted storage parameters,
// and never autovacuum_enabled = false, whoever produced the SQL.
func checkReloptionAllowlist(upper string) error {
	stmt, ok := pgconf.ParseAlterTableReloptions(upper)
	if !ok {
		return fmt.Errorf("%w: cannot parse ALTER TABLE storage parameters", ErrDisallowedSQL)
	}
	for _, opt := range stmt.Options {
		if err := pgconf.CheckExecutableReloption(opt, stmt.Reset); err != nil {
			return fmt.Errorf("%w: %v", ErrDisallowedSQL, err)
		}
	}
	return nil
}
