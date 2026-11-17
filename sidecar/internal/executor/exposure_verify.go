package executor

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/extschema"
	"github.com/pg-sage/sidecar/internal/verify"
	"github.com/pg-sage/sidecar/internal/workload"
)

// Reader verification of a PUBLIC revoke (fleetpol-02b). pg_stat_statements
// counts only statements that succeed, so a reader that relied on PUBLIC
// and now gets "permission denied" stops adding calls. The readers are the
// statements that reference the table; those that ran in the hour before
// the revoke (pg_sage's statement history) must keep running in the window
// after it (live calls against the counts taken at the revoke), or the
// revoke is reverted through the standing gate's rollback path.

const (
	defaultRevokeVerifyWindow = 15 * time.Minute
	revokeReaderLookback      = time.Hour
	revokeReaderLimit         = 500
)

// revokeVerifyWindow is the window after a revoke, or the default.
func revokeVerifyWindow(configured time.Duration) time.Duration {
	if configured <= 0 {
		return defaultRevokeVerifyWindow
	}
	return configured
}

// judgeRevokeReaders: a statement active before (calls > 0) with no calls
// after is a stopped reader. No active reader is no evidence of harm.
func judgeRevokeReaders(before, after map[int64]verify.Measurement) (bool, string) {
	var stopped []int64
	for id, m := range before {
		if m.Samples > 0 && after[id].Samples <= 0 {
			stopped = append(stopped, id)
		}
	}
	if len(stopped) == 0 {
		return false, fmt.Sprintf("%d reader statement(s) kept running", len(before))
	}
	slices.Sort(stopped)
	return true, fmt.Sprintf("reader statement(s) %v ran in the hour before the revoke "+
		"and stopped after it", stopped)
}

// revokeReaderCalls are the calls of each statement that references the
// table. nil means pg_stat_statements could not be read: there is no reader
// evidence (empty means no statement references the table).
func (e *Executor) revokeReaderCalls(ctx context.Context, stmt exposureStmt) map[int64]int64 {
	stmts, ok := e.tableStatements(ctx, stmt)
	if !ok {
		return nil
	}
	out := make(map[int64]int64, len(stmts))
	for _, s := range stmts {
		out[s.QueryID] = s.Calls
	}
	return out
}

// tableStatements are the pg_stat_statements entries of this database that
// reference the table, by calls; the query text is read only to match the
// table and is never kept. ok is false when pg_stat_statements is unreadable.
func (e *Executor) tableStatements(
	ctx context.Context, stmt exposureStmt,
) (out []exposureStatement, ok bool) {
	if e.pool == nil {
		return nil, false
	}
	rel, err := extschema.StatementsRel(ctx, e.pool, "pg_stat_statements")
	if err != nil {
		e.logFn("executor", "exposure readers of %s: %v", stmt.qualified(), err)
		return nil, false
	}
	rows, err := e.pool.Query(ctx, `/* pg_sage executor */ SELECT queryid, calls, query
		FROM `+rel+` WHERE dbid = (SELECT oid FROM pg_catalog.pg_database
		  WHERE datname = pg_catalog.current_database()) AND queryid IS NOT NULL
		ORDER BY calls DESC LIMIT $1`, revokeReaderLimit)
	if err != nil {
		e.logFn("executor", "exposure readers of %s: %v", stmt.qualified(), err)
		return nil, false
	}
	defer rows.Close()
	known := map[string][]string{stmt.table: {stmt.schema}}
	for rows.Next() {
		var s exposureStatement
		var query string
		if err := rows.Scan(&s.QueryID, &s.Calls, &query); err != nil {
			e.logFn("executor", "exposure readers of %s: %v", stmt.qualified(), err)
			return nil, false
		}
		if refs := workload.ReferencedTables(query, known); len(refs) > 0 {
			out = append(out, s)
		}
	}
	if err := rows.Err(); err != nil {
		e.logFn("executor", "exposure readers of %s: %v", stmt.qualified(), err)
		return nil, false
	}
	return out, true
}

// startRevokeWatch watches the readers of an executed revoke in the
// background; Shutdown aborts it.
func (e *Executor) startRevokeWatch(ctx context.Context, f analyzer.Finding, actionID int64,
	stmt exposureStmt, readers map[int64]int64) {
	at := time.Now()
	e.startRollbackMonitor(func() {
		e.watchRevokeReaders(context.WithoutCancel(ctx), f, actionID, stmt, readers, at)
	})
}

func (e *Executor) watchRevokeReaders(ctx context.Context, f analyzer.Finding,
	actionID int64, stmt exposureStmt, readers map[int64]int64, at time.Time) {
	if readers == nil {
		e.verifyRevokeKept(ctx, actionID, "unverifiable",
			"pg_stat_statements was unreadable at the revoke: no reader evidence")
		return
	}
	ids := make([]int64, 0, len(readers))
	for id := range readers {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	before, err := e.exposureHistoryFn()(ctx, ids, at.Add(-revokeReaderLookback), at)
	if err != nil {
		e.verifyRevokeKept(ctx, actionID, "unverifiable",
			"no statement history for the hour before: "+err.Error())
		return
	}
	window := revokeVerifyWindow(e.revokeWindow)
	if !waitRollbackWindow(ctx, e.pool, actionID, window, e.logFn, e.shutdownCh) {
		return
	}
	now := e.revokeReaderCalls(ctx, stmt)
	if now == nil {
		e.verifyRevokeKept(ctx, actionID, "unverifiable",
			"pg_stat_statements was unreadable after the window: no reader evidence")
		return
	}
	after := make(map[int64]verify.Measurement, len(ids))
	for _, id := range ids {
		after[id] = verify.Measurement{Samples: int(now[id] - readers[id])}
	}
	regressed, reason := judgeRevokeReaders(before, after)
	if !regressed {
		e.verifyRevokeKept(ctx, actionID, "success", reason)
		return
	}
	e.logFn("executor", "revoke of action %d reverted: %s", actionID, reason)
	rollbackRegressedAction(ctx, e.pool, actionID, f.RollbackSQL,
		e.findingMonitorConfig(f, actionID), e.logFn)
}

// verifyRevokeKept settles a kept revoke: verified, or unverifiable when
// there was no reader evidence to judge.
func (e *Executor) verifyRevokeKept(ctx context.Context, actionID int64, verdict, reason string) {
	if verdict == "success" {
		updateActionSuccess(ctx, e.pool, actionID)
		return
	}
	setMonitoredOutcome(ctx, e.pool, actionID, verdict, reason)
	if _, err := finalizeActionVerification(ctx, e.pool, actionID, verdict,
		reason); err != nil {
		e.logFn("executor", "settle revoke action %d: %v", actionID, err)
	}
}

// exposureHistoryFn is the statement history the hour before is read from.
func (e *Executor) exposureHistoryFn() queryObserver {
	if e.exposureHistory != nil {
		return e.exposureHistory
	}
	return verify.NewPostgresObservationSource(e.pool).QueryMeasurements
}

// rlsCard is the approval card of an RLS enable: the statements that
// reference the table and the roles that would lose access.
func (e *Executor) rlsCard(ctx context.Context, stmt exposureStmt) map[string]any {
	stmts, _ := e.tableStatements(ctx, stmt) // unreadable: the card lists none
	return rlsApprovalCard(stmt.schema, stmt.table, stmts, e.rlsRolesLosingAccess(ctx, stmt))
}

// rlsRolesLosingAccess are the grantees of the table (PUBLIC included) that
// neither own it nor bypass RLS: with RLS on and no policy they read nothing.
func (e *Executor) rlsRolesLosingAccess(ctx context.Context, stmt exposureStmt) []string {
	rows, err := e.actionDB().Query(ctx, `/* pg_sage executor */ SELECT DISTINCT
		  CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE r.rolname::text END
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		CROSS JOIN LATERAL pg_catalog.aclexplode(COALESCE(c.relacl,
		  pg_catalog.acldefault('r', c.relowner))) a
		LEFT JOIN pg_catalog.pg_roles r ON r.oid = a.grantee
		WHERE n.nspname = $1 AND c.relname = $2 AND a.grantee <> c.relowner
		  AND (a.grantee = 0 OR NOT (r.rolsuper OR r.rolbypassrls))
		ORDER BY 1`, stmt.schema, stmt.table)
	if err != nil {
		e.logFn("executor", "roles losing access to %s: %v", stmt.qualified(), err)
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			return out
		}
		out = append(out, role)
	}
	return out
}
