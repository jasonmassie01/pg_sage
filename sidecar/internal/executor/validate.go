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
		if strings.HasPrefix(upper, prefix) {
			return nil
		}
	}

	return fmt.Errorf(
		"%w: statement must start with one of the "+
			"allowed prefixes (CREATE INDEX, DROP INDEX, "+
			"REINDEX, VACUUM, ANALYZE, ALTER TABLE, "+
			"ALTER SYSTEM, ALTER DATABASE, SET, RESET, SELECT)",
		ErrDisallowedSQL,
	)
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
