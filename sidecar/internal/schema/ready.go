package schema

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrSchemaNotReady reports a sage schema that Bootstrap has not (fully)
// created: the readiness endpoint answers 503 until it has.
var ErrSchemaNotReady = errors.New("sage schema not ready")

// readinessSQL returns the expected tables missing from the sage schema
// (kind 'table'), and idx_config_key_db (kind 'index') unless it exists and
// is valid and unique: without it every sage.config upsert fails (config-04).
// The pg_class lookup is by (relname, relnamespace), an index scan at any
// catalog size.
const readinessSQL = `/* pg_sage readiness v3 */
SELECT 'table', t.name
  FROM unnest($1::text[]) AS t(name)
 WHERE NOT EXISTS (
       SELECT 1
         FROM pg_catalog.pg_class c
         JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'sage'
          AND c.relname = t.name
          AND c.relkind IN ('r', 'p'))
UNION ALL
SELECT 'index', 'idx_config_key_db'
 WHERE NOT EXISTS (
       SELECT 1 FROM pg_catalog.pg_index i
        WHERE i.indexrelid = pg_catalog.to_regclass('sage.idx_config_key_db')
          AND i.indisvalid AND i.indisunique)
 ORDER BY 1 DESC, 2`

// readinessTables lists the tables Bootstrap guarantees, in its order.
func readinessTables() []string {
	names := make([]string, 0, len(expectedTables))
	for _, table := range expectedTables {
		names = append(names, table.name)
	}
	return names
}

// CheckReady returns nil when every table Bootstrap creates exists and the
// sage.config key index is valid and unique, an error wrapping
// ErrSchemaNotReady naming what is missing, or the query error.
func CheckReady(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("schema readiness requires a PostgreSQL connection pool")
	}
	rows, err := pool.Query(ctx, readinessSQL, readinessTables())
	if err != nil {
		return fmt.Errorf("checking sage schema readiness: %w", err)
	}
	defer rows.Close()
	missing := map[string][]string{}
	for rows.Next() {
		var kind, name string
		if err := rows.Scan(&kind, &name); err != nil {
			return fmt.Errorf("reading sage schema readiness: %w", err)
		}
		missing[kind] = append(missing[kind], name)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("checking sage schema readiness: %w", err)
	}
	var parts []string
	if len(missing["table"]) > 0 {
		parts = append(parts, "missing tables "+strings.Join(missing["table"], ", "))
	}
	if len(missing["index"]) > 0 {
		parts = append(parts, "missing, invalid or non-unique index sage."+
			strings.Join(missing["index"], ", sage."))
	}
	if len(parts) > 0 {
		return fmt.Errorf("%w: %s", ErrSchemaNotReady, strings.Join(parts, "; "))
	}
	return nil
}
