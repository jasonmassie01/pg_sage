package selfcost

import (
	"context"
	"errors"
	"fmt"
)

// TableSize is one sage table's total size (heap, TOAST, indexes).
type TableSize struct {
	Name  string
	Bytes int64
}

// Schema is one reading of the sage schema in a database: its tables'
// sizes and statistics counters. It is the one source of the schema's size
// in a cycle: the footprint check, the self-cost meter and the storage
// budget all use the same reading.
type Schema struct {
	Database    string
	Tables      []TableSize // largest first
	Bytes       int64       // the tables' sum
	RowsRead    int64       // sequential + index tuple reads
	RowsWritten int64       // rows inserted + updated + deleted
}

// sageTablesSQL lists the sage tables with their sizes and counters. The
// left join keeps one row (the database name, NULL table) when the schema
// has no table.
const sageTablesSQL = `/* pg_sage */
SELECT d.db, t.name, t.bytes, t.rows_read, t.rows_written
  FROM (SELECT current_database()) d(db)
  LEFT JOIN LATERAL (
    SELECT n.nspname || '.' || c.relname AS name,
           pg_total_relation_size(c.oid) AS bytes,
           (pg_stat_get_tuples_returned(c.oid) + pg_stat_get_tuples_fetched(c.oid)
             + COALESCE((SELECT sum(pg_stat_get_tuples_fetched(i.indexrelid))
                           FROM pg_index i WHERE i.indrelid = c.oid), 0))::int8 AS rows_read,
           (pg_stat_get_tuples_inserted(c.oid) + pg_stat_get_tuples_updated(c.oid)
             + pg_stat_get_tuples_deleted(c.oid))::int8 AS rows_written
      FROM pg_class c
      JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE n.nspname = 'sage' AND c.relkind IN ('r', 'm')
  ) t ON true
 ORDER BY t.bytes DESC NULLS LAST, t.name`

var errNoSchemaRow = errors.New("the sage tables read returned no row")

// ReadSchema reads the sage schema's tables in one statement. Run it
// through a bounded reader: pg_total_relation_size waits for the lock of
// a table held by a migration or a retention job.
func ReadSchema(ctx context.Context, q Querier) (Schema, error) {
	sc, err := readSchema(ctx, q)
	if err != nil {
		return Schema{}, fmt.Errorf("read sage schema: %w", err)
	}
	return sc, nil
}

func readSchema(ctx context.Context, q Querier) (Schema, error) {
	var sc Schema
	rows, err := q.Query(ctx, sageTablesSQL)
	if err != nil {
		return sc, err
	}
	defer rows.Close()
	seen := false
	for rows.Next() {
		var name *string
		var bytes, read, written *int64
		if err := rows.Scan(&sc.Database, &name, &bytes, &read, &written); err != nil {
			return sc, fmt.Errorf("scan sage table: %w", err)
		}
		seen = true
		if name == nil {
			continue // no sage table
		}
		sc.Tables = append(sc.Tables, TableSize{Name: *name, Bytes: deref(bytes)})
		sc.Bytes += deref(bytes)
		sc.RowsRead += deref(read)
		sc.RowsWritten += deref(written)
	}
	if err := rows.Err(); err != nil {
		return sc, err
	}
	if !seen {
		return sc, errNoSchemaRow
	}
	return sc, nil
}

func deref(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
