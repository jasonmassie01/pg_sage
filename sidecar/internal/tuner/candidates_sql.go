package tuner

import "github.com/pg-sage/sidecar/internal/workload"

// candidateSQL reads the tuning candidates from rel, the qualified
// pg_stat_statements view.
func candidateSQL(rel string) string {
	return `/* pg_sage tuner */
SELECT queryid, query, calls, mean_exec_time,
       mean_plan_time, temp_blks_read, temp_blks_written, userid::int8
FROM ` + rel + `
WHERE calls >= $1
  AND dbid = (
      SELECT oid FROM pg_database WHERE datname = current_database()
  )
  AND ` + workload.AdviceSQL("query") + `
  AND (mean_exec_time > 100
       OR temp_blks_written > 0
       OR (mean_plan_time > 0
           AND mean_plan_time > mean_exec_time * $2))
ORDER BY mean_exec_time * calls DESC
LIMIT 50`
}
