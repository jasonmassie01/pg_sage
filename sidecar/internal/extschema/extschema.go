// Package extschema names the extension objects pg_sage reads
// (pg_stat_statements, pgstattuple, HypoPG) by the schema each extension
// was created in, so a reader finds them whatever the session's
// search_path is, including the pinned pg_catalog, pg_temp of the
// observation pools (security-01c).
package extschema

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrExtensionAbsent marks an extension object whose extension is not
// installed in the database (an empty schema). The error Rel and Func
// return also carries the SQLSTATE the unqualified name raised before
// (42P01 undefined_table, 42883 undefined_function), so each reader's
// "extension unavailable" degradation path is unchanged.
var ErrExtensionAbsent = errors.New("extension is not installed")

// Schemas holds the schema of each extension pg_sage reads; an empty field
// means the extension is not installed in this database.
type Schemas struct {
	Statements  string // pg_stat_statements
	Pgstattuple string // pgstattuple
	HypoPG      string // hypopg
}

// Querier runs one-row statements: a pool, a connection or a transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// detectSQL reads the three extensions' schemas in one catalog statement;
// it names pg_catalog objects only, so it resolves under any search_path.
const detectSQL = `/* pg_sage */ SELECT
  coalesce(max(n.nspname::text) FILTER (WHERE e.extname = 'pg_stat_statements'), ''),
  coalesce(max(n.nspname::text) FILTER (WHERE e.extname = 'pgstattuple'), ''),
  coalesce(max(n.nspname::text) FILTER (WHERE e.extname = 'hypopg'), '')
FROM pg_catalog.pg_extension e
JOIN pg_catalog.pg_namespace n ON n.oid = e.extnamespace
WHERE e.extname IN ('pg_stat_statements', 'pgstattuple', 'hypopg')`

// Detect reads where each extension is installed in q's database. A
// failed read is an error, never "absent": the caller cannot tell.
func Detect(ctx context.Context, q Querier) (Schemas, error) {
	if q == nil {
		return Schemas{}, errors.New("detect extension schemas: no database connection")
	}
	var s Schemas
	err := q.QueryRow(ctx, detectSQL).Scan(&s.Statements, &s.Pgstattuple, &s.HypoPG)
	if err != nil {
		return Schemas{}, fmt.Errorf("detect extension schemas (pg_extension query): %w", err)
	}
	return s, nil
}

// Rel returns name qualified by schema and quoted as one identifier each
// ("ext"."pg_stat_statements"). An empty schema returns an error matching
// ErrExtensionAbsent and SQLSTATE 42P01, never "".name or a bare name.
func Rel(schema, name string) (string, error) {
	return qualify(schema, name, "42P01", "relation")
}

// Func is Rel for an extension function (pgstattuple_approx,
// pg_stat_statements(false)); its absent error carries 42883.
func Func(schema, name string) (string, error) {
	return qualify(schema, name, "42883", "function")
}

func qualify(schema, name, code, kind string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("qualify extension %s: empty object name", kind)
	}
	if schema == "" {
		return "", absentError{name: name, code: code, kind: kind}
	}
	return pgx.Identifier{schema, name}.Sanitize(), nil
}

// StatementsRel detects pg_stat_statements' schema on q and returns name
// (the view or its info view) qualified by it. Readers that
// hold no startup-detected schema call it on the session they read
// through, so each database of a fleet resolves its own schema.
func StatementsRel(ctx context.Context, q Querier, name string) (string, error) {
	s, err := Detect(ctx, q)
	if err != nil {
		return "", err
	}
	return Rel(s.Statements, name)
}

// absentError is ErrExtensionAbsent with the undefined_table or
// undefined_function error the bare name raised before.
type absentError struct{ name, code, kind string }

func (e absentError) Error() string {
	return fmt.Sprintf("%s: %v", e.name, ErrExtensionAbsent)
}

func (e absentError) Unwrap() []error {
	return []error{ErrExtensionAbsent, &pgconn.PgError{Code: e.code, Severity: "ERROR",
		Message: fmt.Sprintf("%s %q does not exist: its extension is not installed",
			e.kind, e.name)}}
}
