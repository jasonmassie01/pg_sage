// Package rolegrants holds the privilege checks that both the startup
// grants check and the onboarding "Grant more" guide use, so the two always
// ask for the same grant with the same SQL.
package rolegrants

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Need is what CREATE on a schema is for: PostgreSQL checks CREATE on the
// table's schema for CREATE INDEX and CREATE STATISTICS, even when the role
// owns the table.
const Need = "CREATE INDEX and CREATE STATISTICS on its tables"

// ErrNoPool is returned when there is no database to check.
var ErrNoPool = errors.New("rolegrants: no database connection pool")

// SchemaCreate is the connected role's CREATE privilege on the schemas that
// hold user tables, the only schemas pg_sage creates indexes and statistics
// in. Names are quoted as SQL needs them.
type SchemaCreate struct {
	Schemas []string `json:"schemas"`
	Missing []string `json:"missing"`
}

// schemaCreateSQL lists the schemas holding user tables (ordinary or
// partitioned, not part of an extension), outside the system schemas and
// pg_sage's own sage schema, and whether the current role may CREATE there.
const schemaCreateSQL = `/* pg_sage grants */ SELECT pg_catalog.quote_ident(n.nspname),
  pg_catalog.has_schema_privilege(n.oid, 'CREATE')
FROM pg_catalog.pg_namespace n
WHERE n.nspname NOT IN ('pg_catalog', 'information_schema', 'sage')
  AND n.nspname !~ '^pg_toast' AND n.nspname !~ '^pg_temp'
  AND EXISTS (SELECT 1 FROM pg_catalog.pg_class c
    WHERE c.relnamespace = n.oid AND c.relkind IN ('r', 'p')
      AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d
        WHERE d.classid = 'pg_catalog.pg_class'::pg_catalog.regclass
          AND d.objid = c.oid AND d.deptype = 'e'))
ORDER BY n.nspname`

// CheckSchemaCreate reads, live, which schemas holding user tables the
// connected role lacks CREATE on.
func CheckSchemaCreate(ctx context.Context, pool *pgxpool.Pool) (SchemaCreate, error) {
	if pool == nil {
		return SchemaCreate{}, ErrNoPool
	}
	rows, err := pool.Query(ctx, schemaCreateSQL)
	if err != nil {
		return SchemaCreate{}, fmt.Errorf("read schema CREATE privileges: %w", err)
	}
	defer rows.Close()
	var out SchemaCreate
	for rows.Next() {
		var name string
		var ok bool
		if err := rows.Scan(&name, &ok); err != nil {
			return SchemaCreate{}, fmt.Errorf("read schema CREATE privileges: %w", err)
		}
		out.Schemas = append(out.Schemas, name)
		if !ok {
			out.Missing = append(out.Missing, name)
		}
	}
	if err := rows.Err(); err != nil {
		return SchemaCreate{}, fmt.Errorf("read schema CREATE privileges: %w", err)
	}
	return out, nil
}

// Lacking reports whether CREATE is missing on any schema holding tables.
func (s SchemaCreate) Lacking() bool { return len(s.Missing) > 0 }

// target is the schemas a grant names: the missing ones, else every schema
// holding tables, else public, where new tables go by default.
func (s SchemaCreate) target() []string {
	switch {
	case len(s.Missing) > 0:
		return s.Missing
	case len(s.Schemas) > 0:
		return s.Schemas
	}
	return []string{"public"}
}

// What names the privilege, e.g. "CREATE on schemas app, public".
func (s SchemaCreate) What() string {
	t := s.target()
	if len(t) == 1 {
		return "CREATE on schema " + t[0]
	}
	return "CREATE on schemas " + strings.Join(t, ", ")
}

// GrantSQL is the statement that grants it to role (already quoted).
func (s SchemaCreate) GrantSQL(role string) string {
	return "GRANT CREATE ON SCHEMA " + strings.Join(s.target(), ", ") + " TO " + role
}
