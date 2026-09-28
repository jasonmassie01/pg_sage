package lint

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sanitize"
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

// schemaExcludeSQL returns a SQL IN-list for schema exclusion. Entries
// are emitted as string literals with quotes doubled, so any schema name
// (uppercase, hyphens, spaces) is honoured instead of silently dropped
// (G2-B27). Empty entries and entries containing a backslash or NUL are
// skipped: a backslash could escape the literal on a server running with
// standard_conforming_strings=off, and no real schema needs one.
func schemaExcludeSQL(extra []string) string {
	parts := []string{"'pg_catalog'", "'information_schema'", "'pg_toast'"}
	for _, s := range extra {
		if s == "" || strings.ContainsAny(s, "\\\x00") {
			continue
		}
		parts = append(parts, sanitize.QuoteLiteral(s))
	}
	return strings.Join(parts, ",")
}
