package planstability

import (
	"context"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// The statuses of a table's statistics.
const (
	StatusOK            = "ok"
	StatusNoPrivilege   = "no_privilege"
	StatusNeverAnalyzed = "never_analyzed"
)

// Source reads what the pack does not carry, from the monitored database:
// the leading most_common_freqs value (a frequency, never a value), the
// columns' statistics targets, rows changed since the last ANALYZE, the
// privileges and ownership. A table the source does not list does not
// exist. An error is the read failing, never a table it cannot see.
type Source interface {
	Read(ctx context.Context, tables []string) (Facts, error)
}

// Facts is the catalog of the tables asked for. DefaultTarget is
// default_statistics_target; Role is the role pg_sage reads as.
type Facts struct {
	DefaultTarget int
	Role          string
	Tables        map[string]TableFacts
}

// TableFacts is one table ("schema.name"). Owned says pg_sage's role may
// SET STATISTICS on it. RelTuples is pg_class.reltuples (-1: never
// analyzed or vacuumed on PostgreSQL 14+).
type TableFacts struct {
	Status          string
	RelTuples       float64
	ModSinceAnalyze int64
	Owned           bool
	Columns         map[string]ColumnFacts
}

// ColumnFacts is one column's planner statistics (HasStats false: pg_stats
// has no row for it). StatTarget is pg_attribute.attstattarget: nil or -1
// is the default (NULL on PostgreSQL 17+).
type ColumnFacts struct {
	NullFrac    float64
	NDistinct   float64
	Correlation *float64
	TopFreq     *float64
	StatTarget  *int16
	HasStats    bool
}

// DBSource reads through a bounded read-only reader (catalogread.Reader):
// two statements per call, on the catalogs only, no user data.
type DBSource struct {
	q catalogread.Querier
}

// NewDBSource reads through q.
func NewDBSource(q catalogread.Querier) *DBSource { return &DBSource{q: q} }

const tablesSQL = `/* pg_sage review planstability */
SELECT n.nspname || '.' || c.relname, c.reltuples::float8,
       COALESCE(s.n_mod_since_analyze, 0),
       (s.last_analyze IS NOT NULL OR s.last_autoanalyze IS NOT NULL
        OR EXISTS (SELECT 1 FROM pg_stats p WHERE p.schemaname = n.nspname
                   AND p.tablename = c.relname)),
       has_table_privilege(c.oid, 'SELECT'), pg_has_role(c.relowner, 'USAGE'),
       current_setting('default_statistics_target')::int, current_user::text
  FROM unnest($1::text[], $2::text[]) AS t(schema_name, table_name)
  JOIN pg_namespace n ON n.nspname = t.schema_name
  JOIN pg_class c ON c.relnamespace = n.oid AND c.relname = t.table_name
  LEFT JOIN pg_stat_all_tables s ON s.relid = c.oid
 WHERE c.relkind IN ('r', 'p', 'm')`

const columnsSQL = `/* pg_sage review planstability */
SELECT n.nspname || '.' || c.relname, a.attname::text, a.attstattarget::int2,
       s.null_frac::float8, s.n_distinct::float8, s.correlation::float8,
       (s.most_common_freqs)[1]::float8, s.attname IS NOT NULL
  FROM unnest($1::text[], $2::text[]) AS t(schema_name, table_name)
  JOIN pg_namespace n ON n.nspname = t.schema_name
  JOIN pg_class c ON c.relnamespace = n.oid AND c.relname = t.table_name
  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
  LEFT JOIN pg_stats s ON s.schemaname = n.nspname AND s.tablename = c.relname
                      AND s.attname = a.attname AND NOT s.inherited
 ORDER BY 1, a.attnum
 LIMIT 20000`

// Read states each named table that exists.
func (s *DBSource) Read(ctx context.Context, tables []string) (Facts, error) {
	out := Facts{Tables: map[string]TableFacts{}}
	schemas, names := splitNames(tables)
	if len(schemas) == 0 {
		return out, nil
	}
	if err := s.readTables(ctx, schemas, names, &out); err != nil {
		return out, fmt.Errorf("read tables %s: %w", strings.Join(tables, ", "), err)
	}
	if err := s.readColumns(ctx, schemas, names, &out); err != nil {
		return out, fmt.Errorf("read column statistics of %s: %w", strings.Join(tables, ", "),
			err)
	}
	return out, nil
}

func (s *DBSource) readTables(ctx context.Context, schemas, names []string, out *Facts) error {
	rows, err := s.q.Query(ctx, tablesSQL, schemas, names)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var t TableFacts
		var analyzed, readable bool
		if err := rows.Scan(&name, &t.RelTuples, &t.ModSinceAnalyze, &analyzed, &readable,
			&t.Owned, &out.DefaultTarget, &out.Role); err != nil {
			return err
		}
		t.Status, t.Columns = StatusOK, map[string]ColumnFacts{}
		switch {
		case !readable:
			t.Status = StatusNoPrivilege
		case !analyzed:
			t.Status = StatusNeverAnalyzed
		}
		out.Tables[name] = t
	}
	return rows.Err()
}

func (s *DBSource) readColumns(ctx context.Context, schemas, names []string, out *Facts) error {
	rows, err := s.q.Query(ctx, columnsSQL, schemas, names)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var table, column string
		var c ColumnFacts
		var nullFrac, nDistinct *float64
		if err := rows.Scan(&table, &column, &c.StatTarget, &nullFrac, &nDistinct,
			&c.Correlation, &c.TopFreq, &c.HasStats); err != nil {
			return err
		}
		if nullFrac != nil && nDistinct != nil {
			c.NullFrac, c.NDistinct = *nullFrac, *nDistinct
		}
		if t, ok := out.Tables[table]; ok {
			t.Columns[column] = c
		}
	}
	return rows.Err()
}

// splitNames splits "schema.name" (the pack's spelling: unquoted parts).
func splitNames(tables []string) (schemas, names []string) {
	for _, t := range tables {
		if schema, name, ok := strings.Cut(t, "."); ok && schema != "" && name != "" {
			schemas, names = append(schemas, schema), append(names, name)
		}
	}
	return schemas, names
}
