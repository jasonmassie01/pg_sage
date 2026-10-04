package tuning

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

const extendedStatsSQL = `/* pg_sage */
SELECT s.stxname::text,
       ARRAY(SELECT a.attname::text
             FROM unnest(s.stxkeys::int2[]) WITH ORDINALITY AS k(attnum, ord)
             JOIN pg_catalog.pg_attribute a
               ON a.attrelid = s.stxrelid AND a.attnum = k.attnum
             ORDER BY k.ord),
       ARRAY(SELECT CASE kind WHEN 'd' THEN 'ndistinct' WHEN 'f' THEN 'dependencies'
                              WHEN 'm' THEN 'mcv' ELSE 'expressions' END
             FROM unnest(s.stxkind) AS kind)
FROM pg_catalog.pg_statistic_ext s
JOIN pg_catalog.pg_class c ON c.oid = s.stxrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relname = $2
ORDER BY s.stxname`

func (s *postgresStore) ExtendedStats(ctx context.Context, schema, table string) ([]ExtStat,
	error) {
	rows, err := catalogread.New(s.pool, s.timeouts).Query(ctx, extendedStatsSQL, schema,
		table)
	if err != nil {
		return nil, fmt.Errorf("read extended statistics of %s.%s: %w", schema, table, err)
	}
	defer rows.Close()
	var out []ExtStat
	for rows.Next() {
		var e ExtStat
		if err := rows.Scan(&e.Name, &e.Columns, &e.Kinds); err != nil {
			return nil, fmt.Errorf("scan extended statistics: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read extended statistics of %s.%s: %w", schema, table, err)
	}
	return out, nil
}

const columnStatsSQL = `/* pg_sage */
SELECT attname::text, n_distinct::float8, COALESCE(correlation, 0)::float8,
       null_frac::float8, avg_width
FROM pg_catalog.pg_stats
WHERE schemaname = $1 AND tablename = $2 AND attname = ANY($3)`

func (s *postgresStore) ColumnStats(ctx context.Context, schema, table string, cols []string) (
	[]ColumnStat, error) {
	if len(cols) == 0 {
		return nil, nil
	}
	rows, err := catalogread.New(s.pool, s.timeouts).Query(ctx, columnStatsSQL, schema,
		table, cols)
	if err != nil {
		return nil, fmt.Errorf("read column statistics of %s.%s: %w", schema, table, err)
	}
	defer rows.Close()
	byName := map[string]ColumnStat{}
	for rows.Next() {
		var c ColumnStat
		if err := rows.Scan(&c.Column, &c.NDistinct, &c.Correlation, &c.NullFrac,
			&c.AvgWidth); err != nil {
			return nil, fmt.Errorf("scan column statistics: %w", err)
		}
		byName[c.Column] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read column statistics of %s.%s: %w", schema, table, err)
	}
	out := make([]ColumnStat, 0, len(cols))
	for _, col := range cols {
		if c, ok := byName[col]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}
