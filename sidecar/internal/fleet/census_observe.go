package fleet

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/extschema"
	"github.com/pg-sage/sidecar/internal/store"
)

// workloadCallsSQL sums the calls of this database's pg_stat_statements
// rows run by a role that is not pg_sage's ($1: its login and action
// roles; the session's own role is always left out). %s is the view,
// qualified by its extension's schema (a sanitized identifier, never input).
const workloadCallsSQL = `/* pg_sage census */ SELECT COALESCE(sum(s.calls), 0)::int8
  FROM %s s
 WHERE s.dbid = (SELECT d.oid FROM pg_catalog.pg_database d
                  WHERE d.datname = pg_catalog.current_database())
   AND s.userid NOT IN (SELECT r.oid FROM pg_catalog.pg_roles r
                         WHERE r.rolname = ANY($1::text[]) OR r.rolname = session_user)`

// readObserveActivity is a visit's census reading, taken on the visit's
// read-only connection within timeout: one pg_stat_statements read and
// the database's size. Either is nil when unreadable (pg_stat_statements
// not installed, or not visible); the census then reports activity as
// unknown, and the visit's grade stands.
func readObserveActivity(ctx context.Context, pool *pgxpool.Pool, rec store.DatabaseRecord,
	timeout time.Duration) *ObserveActivity {
	qctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	a := &ObserveActivity{}
	var size int64
	if err := pool.QueryRow(qctx, `/* pg_sage census */ SELECT
		pg_catalog.pg_database_size(pg_catalog.current_database())`).Scan(&size); err == nil {
		a.SizeBytes = &size
	}
	rel, err := extschema.StatementsRel(qctx, pool, "pg_stat_statements")
	if err != nil {
		return a
	}
	roles := []string{rec.Username}
	if rec.ActionUsername != "" {
		roles = append(roles, rec.ActionUsername)
	}
	var calls int64
	if err := pool.QueryRow(qctx, fmt.Sprintf(workloadCallsSQL, rel), roles).Scan(
		&calls); err == nil {
		a.WorkloadCalls = &calls
	}
	return a
}
