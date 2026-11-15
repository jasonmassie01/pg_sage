package backtest

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/snapstore"
	"github.com/pg-sage/sidecar/internal/verify"
	"github.com/pg-sage/sidecar/internal/workload"
)

// PostgresHistory is a database's stored history: counters through
// verify's CounterSource (the snapshot queries category and the hourly
// rollup in the history store), the statements and tables of the latest
// snapshots, and sage.action_log. Every statement only reads.
type PostgresHistory struct {
	*verify.CounterSource
	pool *pgxpool.Pool
}

// NewPostgresHistory reads pool's history.
func NewPostgresHistory(pool *pgxpool.Pool) *PostgresHistory {
	return &PostgresHistory{CounterSource: verify.NewCounterSource(pool), pool: pool}
}

const spanSQL = `/* pg_sage */ SELECT min(s.collected_at), max(s.collected_at)
FROM sage.snapshots s WHERE {db:s} AND s.category = 'queries'`

// Span is the first and last queries snapshot.
func (h *PostgresHistory) Span(ctx context.Context) (time.Time, time.Time, bool, error) {
	var from, to *time.Time
	if err := histstore.Resolve(h.pool).QueryRow(ctx, spanSQL).Scan(&from, &to); err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	if from == nil || to == nil {
		return time.Time{}, time.Time{}, false, nil
	}
	return *from, *to, true, nil
}

// latestDocSQL is the elements of the newest snapshot of category $1 at
// or before $2, with the columns cols.
func latestDocSQL(cols string) string {
	return `/* pg_sage */ SELECT r.* FROM (SELECT ` + snapstore.DataSQL("s") + ` AS doc
	FROM sage.snapshots s WHERE {db:s} AND s.category = $1 AND s.collected_at <= $2
	ORDER BY s.collected_at DESC LIMIT 1) l
	CROSS JOIN LATERAL jsonb_to_recordset(CASE WHEN jsonb_typeof(l.doc) = 'array'
		THEN l.doc ELSE '[]'::jsonb END) AS r(` + cols + `)`
}

// knownTables is the newest tables snapshot at or before at, as
// workload.ReferencedTables reads it.
func (h *PostgresHistory) knownTables(ctx context.Context, at time.Time) (
	map[string][]string, error) {
	rows, err := histstore.Resolve(h.pool).Query(ctx,
		latestDocSQL("schemaname text, relname text"), "tables", at)
	if err != nil {
		return nil, fmt.Errorf("read the tables snapshot: %w", err)
	}
	known := map[string][]string{}
	var schema, name *string
	_, err = pgx.ForEachRow(rows, []any{&schema, &name}, func() error {
		if schema != nil && name != nil {
			known[*name] = append(known[*name], *schema)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the tables snapshot: %w", err)
	}
	return known, nil
}

// Statements are the workload statements of the newest queries snapshot
// at or before at (pg_sage's own and diagnostics left out), each with the
// tables it names.
func (h *PostgresHistory) Statements(ctx context.Context, at time.Time) ([]Statement,
	error) {
	known, err := h.knownTables(ctx, at)
	if err != nil {
		return nil, err
	}
	rows, err := histstore.Resolve(h.pool).Query(ctx,
		latestDocSQL("queryid int8, query text, calls int8"), "queries", at)
	if err != nil {
		return nil, fmt.Errorf("read the queries snapshot: %w", err)
	}
	var out []Statement
	var id, calls *int64
	var query *string
	_, err = pgx.ForEachRow(rows, []any{&id, &query, &calls}, func() error {
		if id == nil || *id == 0 || calls == nil || *calls <= 0 || query == nil ||
			workload.Excluded(*query) {
			return nil
		}
		s := Statement{QueryID: *id, Calls: *calls}
		for _, r := range workload.ReferencedTables(*query, known) {
			s.Tables = append(s.Tables, r.Schema+"."+r.Name)
		}
		out = append(out, s)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the queries snapshot: %w", err)
	}
	return out, nil
}

// actionsSQL is every action of sage.action_log in [$1, $2), whatever its
// outcome: a pending row may be one cut off mid-run (a retention delete is
// recorded pending before it runs) and a failed one may have loaded the
// database (a CREATE INDEX CONCURRENTLY that failed late), so a placebo
// keeps its gap from all of them.
const actionsSQL = `/* pg_sage */ SELECT executed_at, sql_executed FROM sage.action_log
WHERE executed_at >= $1 AND executed_at < $2
ORDER BY executed_at, id`

// Actions are the actions of sage.action_log in [from, to). An action
// touches each known table whose name its SQL names as a word; one that
// names none may have touched any.
func (h *PostgresHistory) Actions(ctx context.Context, from, to time.Time) ([]Action,
	error) {
	known, err := h.knownTables(ctx, to)
	if err != nil {
		return nil, err
	}
	rows, err := h.pool.Query(ctx, actionsSQL, from, to)
	if err != nil {
		return nil, fmt.Errorf("read the executed actions: %w", err)
	}
	words := tableWords(known)
	var out []Action
	var at time.Time
	var sql string
	_, err = pgx.ForEachRow(rows, []any{&at, &sql}, func() error {
		out = append(out, Action{At: at.UTC(), Tables: touched(sql, words)})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the executed actions: %w", err)
	}
	return out, nil
}

// tableWord matches a known table's name as a word of an action's SQL,
// case-insensitively, and names the tables (schema.name) it stands for.
type tableWord struct {
	re     *regexp.Regexp
	tables []string
}

// tableWords compiles one matcher per known table name, once per read.
func tableWords(known map[string][]string) []tableWord {
	out := make([]tableWord, 0, len(known))
	for name, schemas := range known {
		w := tableWord{re: regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_$])"?` +
			regexp.QuoteMeta(name) + `"?($|[^A-Za-z0-9_$])`)}
		for _, s := range schemas {
			w.tables = append(w.tables, s+"."+name)
		}
		out = append(out, w)
	}
	return out
}

// touched are the known tables (schema.name) whose name sql names as a
// word.
func touched(sql string, words []tableWord) []string {
	var out []string
	for _, w := range words {
		if w.re.MatchString(sql) {
			out = append(out, w.tables...)
		}
	}
	slices.Sort(out)
	return out
}
