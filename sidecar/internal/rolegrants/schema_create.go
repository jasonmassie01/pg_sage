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

	"github.com/pg-sage/sidecar/internal/sqlident"
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
const schemaCreateSQL = `/* pg_sage grants */ SELECT n.nspname::text,
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
		quoted := sqlident.Ident(name)
		out.Schemas = append(out.Schemas, quoted)
		if !ok {
			out.Missing = append(out.Missing, quoted)
		}
	}
	if err := rows.Err(); err != nil {
		return SchemaCreate{}, fmt.Errorf("read schema CREATE privileges: %w", err)
	}
	return out, nil
}

// Lacking reports whether CREATE is missing on any schema holding tables.
func (s SchemaCreate) Lacking() bool { return len(s.Missing) > 0 }

// What names the privilege, e.g. "CREATE on schemas app, public".
func (s SchemaCreate) What() string {
	return "CREATE on " + schemaList(schemaTarget(s.Missing, s.Schemas))
}

// GrantSQL is the statement that grants it to role (already quoted).
func (s SchemaCreate) GrantSQL(role string) string {
	return "GRANT CREATE ON SCHEMA " + strings.Join(schemaTarget(s.Missing, s.Schemas), ", ") +
		" TO " + role
}
