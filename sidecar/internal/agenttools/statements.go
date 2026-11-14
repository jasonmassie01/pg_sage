package agenttools

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/extschema"
)

// statement is one pg_stat_statements query of the current database,
// summed over users and top-level/nested entries.
type statement struct {
	ID          int64
	Text        string
	Calls       int64
	TotalTimeMs float64
	Rows        int64
}

func (s statement) meanMs() float64 {
	if s.Calls <= 0 {
		return 0
	}
	return s.TotalTimeMs / float64(s.Calls)
}

const statementColumns = `queryid, min(query), sum(calls)::bigint,
	sum(total_exec_time)::float8, sum(rows)::bigint`

// statementsByIDSQL reads the ids' statements from rel, the qualified
// pg_stat_statements view.
func statementsByIDSQL(rel string) string {
	return `/* pg_sage */ SELECT ` + statementColumns + `
FROM ` + rel + `
WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND queryid = ANY($1)
GROUP BY queryid`
}

// statementsByID reads the statements with the given ids; ids without a
// statement are absent from the map.
func (t *Tools) statementsByID(ctx context.Context, ids []int64,
) (map[int64]statement, error) {
	rel, err := extschema.StatementsRel(ctx, t.pool, "pg_stat_statements")
	if err != nil {
		return nil, statementsError(err)
	}
	rows, err := t.pool.Query(ctx, statementsByIDSQL(rel), ids)
	if err != nil {
		return nil, statementsError(err)
	}
	defer rows.Close()
	out := make(map[int64]statement, len(ids))
	for rows.Next() {
		var s statement
		if err := rows.Scan(&s.ID, &s.Text, &s.Calls, &s.TotalTimeMs, &s.Rows); err != nil {
			return nil, fmt.Errorf("scan pg_stat_statements: %w", err)
		}
		out[s.ID] = s
	}
	if err := rows.Err(); err != nil {
		return nil, statementsError(err)
	}
	return out, nil
}

// statementsError maps a missing or unloaded pg_stat_statements to
// ErrUnavailable.
func statementsError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "42P01", "42883", "55000", "3F000":
			return fmt.Errorf("%w: pg_stat_statements is not usable here: %s",
				ErrUnavailable, pgErr.Message)
		}
	}
	return fmt.Errorf("read pg_stat_statements: %w", err)
}
