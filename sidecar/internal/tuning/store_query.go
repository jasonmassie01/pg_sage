package tuning

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/extschema"
)

// queryStatesSQL reads, for each queryid in $1, whether pg_stat_statements
// holds it in this database, whether the query tuner's admission
// (candidateSQL: calls >= $2 and a mean above 100 ms, temp blocks written,
// or planning above $3 times execution) would take it now, and the status
// of its latest sage.query_hints row. pg_stat_statements is read once.
func queryStatesSQL(rel string) string {
	return `/* pg_sage */
WITH s AS MATERIALIZED (
    SELECT queryid, calls, mean_exec_time, mean_plan_time, temp_blks_written
    FROM ` + rel + `
    WHERE queryid = ANY($1)
      AND dbid = (SELECT oid FROM pg_catalog.pg_database
                  WHERE datname = pg_catalog.current_database()))
SELECT ids.qid,
       EXISTS (SELECT 1 FROM s WHERE s.queryid = ids.qid),
       EXISTS (SELECT 1 FROM s WHERE s.queryid = ids.qid AND s.calls >= $2
                  AND (s.mean_exec_time > 100 OR s.temp_blks_written > 0
                       OR (s.mean_plan_time > 0
                           AND s.mean_plan_time > s.mean_exec_time * $3))),
       COALESCE((SELECT q.status FROM sage.query_hints q WHERE q.queryid = ids.qid
                 ORDER BY q.id DESC LIMIT 1), '')
FROM unnest($1::bigint[]) AS ids(qid)`
}

func (s *postgresStore) QueryStates(ctx context.Context, ids []int64, adm HintAdmission) (
	map[int64]QueryState, error) {
	out := map[int64]QueryState{}
	if len(ids) == 0 {
		return out, nil
	}
	db := catalogread.New(s.pool, s.timeouts)
	rel, err := extschema.StatementsRel(ctx, db, "pg_stat_statements")
	var rows pgx.Rows
	if err == nil {
		rows, err = db.Query(ctx, queryStatesSQL(rel), ids, adm.MinCalls, adm.PlanTimeRatio)
	}
	if err != nil {
		return nil, fmt.Errorf("read statement states from pg_stat_statements and "+
			"sage.query_hints: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var st QueryState
		if err := rows.Scan(&id, &st.Present, &st.Admitted, &st.HintStatus); err != nil {
			return nil, fmt.Errorf("scan statement state: %w", err)
		}
		out[id] = st
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read statement states: %w", err)
	}
	return out, nil
}
