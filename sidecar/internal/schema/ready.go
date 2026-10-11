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

// readinessSQL returns the expected tables missing from the sage schema. The
// pg_class lookup is by (relname, relnamespace), an index scan at any
// catalog size.
const readinessSQL = `/* pg_sage readiness v1 */
SELECT t.name
  FROM unnest($1::text[]) AS t(name)
 WHERE NOT EXISTS (
       SELECT 1
         FROM pg_catalog.pg_class c
         JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'sage'
          AND c.relname = t.name
          AND c.relkind IN ('r', 'p'))
 ORDER BY t.name`

// readinessTables lists the tables Bootstrap guarantees, in its order.
func readinessTables() []string {
	names := make([]string, 0, len(expectedTables))
	for _, table := range expectedTables {
		names = append(names, table.name)
	}
	return names
}

// CheckReady returns nil when every table Bootstrap creates exists, an error
// wrapping ErrSchemaNotReady naming the missing ones, or the query error.
func CheckReady(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("schema readiness requires a PostgreSQL connection pool")
	}
	rows, err := pool.Query(ctx, readinessSQL, readinessTables())
	if err != nil {
		return fmt.Errorf("checking sage schema readiness: %w", err)
	}
	defer rows.Close()
	var missing []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("reading sage schema readiness: %w", err)
		}
		missing = append(missing, name)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("checking sage schema readiness: %w", err)
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: missing tables %s", ErrSchemaNotReady,
			strings.Join(missing, ", "))
	}
	return nil
}
