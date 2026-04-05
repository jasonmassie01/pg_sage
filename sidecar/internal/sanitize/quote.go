package sanitize

import (
	"fmt"
	"strings"
)

// QuoteIdentifier quotes a PostgreSQL identifier (table, index,
// column name) by doubling any embedded double-quotes and
// wrapping in double-quotes.
func QuoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// QuoteQualifiedName quotes a schema.table pair.
func QuoteQualifiedName(schema, name string) string {
	return QuoteIdentifier(schema) + "." + QuoteIdentifier(name)
}

// QuoteLiteral quotes a PostgreSQL string literal by doubling
// any embedded single-quotes and wrapping in single-quotes.
func QuoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// RejectMultiStatement returns an error if sql contains
// multiple statements.
func RejectMultiStatement(sql string) error {
	idx := strings.Index(sql, ";")
	if idx < 0 {
		return nil
	}
	rest := strings.TrimSpace(sql[idx+1:])
	if rest != "" {
		return fmt.Errorf("multi-statement SQL rejected")
	}
	return nil
}
