package lint

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Rule is the interface every schema lint check must implement.
type Rule interface {
	ID() string
	Name() string
	Severity() string
	Category() string
	Check(ctx context.Context, pool *pgxpool.Pool, opts RuleOpts) ([]Finding, error)
}

// RuleOpts carries runtime parameters that rules may need.
type RuleOpts struct {
	MinTableRows   int
	PGVersionNum   int
	ExcludeSchemas []string
}

// schemaExcludeSQL returns a SQL IN-list for schema exclusion.
// Schemas are quoted to prevent injection.
func schemaExcludeSQL(extra []string) string {
	schemas := []string{"pg_catalog", "information_schema", "pg_toast"}
	for _, s := range extra {
		// Only allow simple identifiers.
		safe := true
		for _, c := range s {
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_') {
				safe = false
				break
			}
		}
		if safe && s != "" {
			schemas = append(schemas, s)
		}
	}
	parts := make([]string, len(schemas))
	for i, s := range schemas {
		parts[i] = "'" + s + "'"
	}
	return strings.Join(parts, ",")
}
