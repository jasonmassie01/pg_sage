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

const relationsSQL = `/* pg_sage */
SELECT r.ref, r.kind, c.oid IS NOT NULL,
       ARRAY(SELECT pg_catalog.pg_get_indexdef(i.indexrelid)
             FROM pg_catalog.pg_index i
             WHERE r.kind = 't' AND i.indrelid = c.oid AND i.indisvalid
             ORDER BY 1)
FROM unnest($1::text[], $2::text[]) AS r(ref, kind)
LEFT JOIN pg_catalog.pg_class c ON c.oid = pg_catalog.to_regclass(r.ref)`

func (s *postgresStore) Relations(ctx context.Context, tables, indexes []string) (
	CatalogState, error) {
	st := CatalogState{Tables: map[string]bool{}, Indexes: map[string]bool{},
		IndexDefs: map[string][]string{}}
	refs, kinds, asked := relationRefs(tables, indexes)
	if len(refs) == 0 {
		return st, nil
	}
	rows, err := catalogread.New(s.pool, s.timeouts).Query(ctx, relationsSQL, refs, kinds)
	if err != nil {
		return CatalogState{}, fmt.Errorf("read relations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ref, kind string
		var exists bool
		var defs []string
		if err := rows.Scan(&ref, &kind, &exists, &defs); err != nil {
			return CatalogState{}, fmt.Errorf("scan relations: %w", err)
		}
		for _, name := range asked[kind+ref] {
			if kind == "t" {
				st.Tables[name], st.IndexDefs[name] = exists, defs
			} else {
				st.Indexes[name] = exists
			}
		}
	}
	if err := rows.Err(); err != nil {
		return CatalogState{}, fmt.Errorf("read relations: %w", err)
	}
	return st, nil
}

// relationRefs are the canonical names to look up, their kinds ("t", "i")
// and the names asked for each; names that do not parse are left out.
func relationRefs(tables, indexes []string) (refs, kinds []string,
	asked map[string][]string) {
	asked = map[string][]string{}
	add := func(kind string, names []string) {
		for _, name := range names {
			ref := canonicalRef(name)
			if ref == "" {
				continue
			}
			if _, seen := asked[kind+ref]; !seen {
				refs, kinds = append(refs, ref), append(kinds, kind)
			}
			asked[kind+ref] = append(asked[kind+ref], name)
		}
	}
	add("t", tables)
	add("i", indexes)
	return refs, kinds, asked
}
