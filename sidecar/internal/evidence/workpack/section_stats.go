package workpack

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// stats.json: pg_stats of the columns of the tables the listed families
// reference. The default read selects null_frac, n_distinct, correlation
// and avg_width only: no column value reaches the pack. Most common values
// and histogram bounds are read only when the build asks for value
// samples (the optimizer's statistics reads are not reused: they read
// most_common_vals).

// statRow is one column's statistics.
type statRow struct {
	table, column string
	nullFrac      float64
	nDistinct     float64
	corr          *float64
	width         int
	mcv, hist     *string
}

const statsFrom = `
  FROM pg_stats s
 WHERE (s.schemaname::text, s.tablename::text) IN
       (SELECT * FROM unnest($1::text[], $2::text[]))
 ORDER BY s.schemaname, s.tablename, s.attname, s.inherited
 LIMIT `

var statsSQL = `/* pg_sage */ SELECT DISTINCT ON (s.schemaname, s.tablename, s.attname)
       s.schemaname || '.' || s.tablename, s.attname::text, s.null_frac::float8,
       s.n_distinct::float8, s.correlation::float8, s.avg_width, NULL::text, NULL::text` +
	statsFrom + fmt.Sprint(MaxStatsColumns+1)

var statsSamplesSQL = `/* pg_sage */ SELECT DISTINCT ON (s.schemaname, s.tablename, s.attname)
       s.schemaname || '.' || s.tablename, s.attname::text, s.null_frac::float8,
       s.n_distinct::float8, s.correlation::float8, s.avg_width,
       s.most_common_vals::text, s.histogram_bounds::text` +
	statsFrom + fmt.Sprint(MaxStatsColumns+1)

func readStats(ctx context.Context, tx pgx.Tx, in catalogInput, cat *catalogData) error {
	if len(in.relations) == 0 {
		return nil
	}
	schemas, names := make([]string, 0, len(in.relations)), make([]string, 0,
		len(in.relations))
	for _, rel := range in.relations {
		schema, name, ok := strings.Cut(rel, ".")
		if ok {
			schemas, names = append(schemas, schema), append(names, name)
		}
	}
	sql := statsSQL
	if in.samples {
		sql = statsSamplesSQL
	}
	rows, err := tx.Query(ctx, sql, schemas, names)
	if err != nil {
		return err
	}
	cat.stats, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (statRow, error) {
		var x statRow
		err := row.Scan(&x.table, &x.column, &x.nullFrac, &x.nDistinct, &x.corr, &x.width,
			&x.mcv, &x.hist)
		return x, err
	})
	return err
}

// buildStats is a status per referenced table (unknown when pg_stats
// shows none of its columns: no table, no privilege or never analyzed)
// and the columns read, at most MaxStatsColumns.
func buildStats(tables []string, rows []statRow, samples bool) Stats {
	out := Stats{Tables: []StatsTable{}, Columns: []ColumnStats{}, ValueSamples: samples}
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.table] = true
	}
	sorted := append([]string(nil), tables...)
	sort.Strings(sorted)
	for _, t := range sorted {
		st := StatsTable{ID: "stats/table/" + t, Table: t, Status: valueUnknown}
		if seen[t] {
			st.Status = relStatusOK
		}
		out.Tables = append(out.Tables, st)
	}
	if len(rows) > MaxStatsColumns {
		rows, out.Truncated = rows[:MaxStatsColumns], true
	}
	for _, r := range rows {
		c := ColumnStats{ID: "stats/column/" + r.table + "." + r.column, Table: r.table,
			Column: r.column, NullFrac: r.nullFrac, NDistinct: r.nDistinct,
			Correlation: r.corr, AvgWidth: r.width}
		if samples {
			c.MostCommonVals, c.HistogramBounds = r.mcv, r.hist
		}
		out.Columns = append(out.Columns, c)
	}
	out.ColumnsListed = len(out.Columns)
	return out
}

// referencedTables are the listed families' relations, sorted, unique.
func referencedTables(wl Workload) []string {
	seen := map[string]bool{}
	var out []string
	for _, fam := range wl.Families {
		for _, rel := range fam.Relations {
			if !seen[rel] {
				seen[rel] = true
				out = append(out, rel)
			}
		}
	}
	sort.Strings(out)
	return out
}
