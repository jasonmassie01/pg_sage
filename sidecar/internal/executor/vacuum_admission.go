package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// vacuum-05: a VACUUM of a table something already vacuums only waits
// behind it on lock_timeout (an anti-wraparound autovacuum is never
// cancelled) or cancels an ordinary autovacuum and loses its work. Such an
// action is withheld before it runs, so no failed action row counts toward
// the custodian backoff.
const (
	reasonVacuumInProgress = "vacuum_in_progress"
	vacuumProgressUnknown  = "progress_unknown"
)

// ErrVacuumInProgress marks a VACUUM withheld because its table is being
// vacuumed.
var ErrVacuumInProgress = errors.New("the table is already being vacuumed")

// runningVacuum is one pg_stat_progress_vacuum row in the current database
// on the target (or its TOAST table), or one whose table the connected role
// cannot see (RelIDKnown false: it lacks pg_read_all_stats).
type runningVacuum struct {
	PID             int
	RelIDKnown      bool
	Phase           string
	HeapBlksScanned int64
	HeapBlksTotal   int64
	Autovacuum      bool
	Wraparound      bool
}

// runningVacuumSQL reads the vacuums of $1 and its TOAST table, plus every
// vacuum in this database whose table is hidden from the role.
const runningVacuumSQL = `/* pg_sage executor */ SELECT p.pid, p.relid IS NOT NULL,
	COALESCE(p.phase, ''), COALESCE(p.heap_blks_scanned, 0), COALESCE(p.heap_blks_total, 0),
	COALESCE(a.backend_type = 'autovacuum worker', false),
	COALESCE(a.query LIKE '%(to prevent wraparound)%', false)
	FROM pg_stat_progress_vacuum p
	LEFT JOIN pg_stat_activity a ON a.pid = p.pid
	WHERE p.datname = current_database() AND p.pid <> pg_backend_pid()
	  AND (p.relid IS NULL OR p.relid = to_regclass($1)
	       OR p.relid = (SELECT c.reltoastrelid FROM pg_class c
	                     WHERE c.oid = to_regclass($1)))
	ORDER BY p.relid IS NULL, p.pid`

func readRunningVacuums(
	ctx context.Context, q catalogread.Querier, table string,
) ([]runningVacuum, error) {
	rows, err := q.Query(ctx, runningVacuumSQL, table)
	if err != nil {
		return nil, fmt.Errorf("read running vacuums of %s: %w", table, err)
	}
	defer rows.Close()
	var out []runningVacuum
	for rows.Next() {
		var v runningVacuum
		if err := rows.Scan(&v.PID, &v.RelIDKnown, &v.Phase, &v.HeapBlksScanned,
			&v.HeapBlksTotal, &v.Autovacuum, &v.Wraparound); err != nil {
			return nil, fmt.Errorf("scan running vacuum of %s: %w", table, err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read running vacuums of %s: %w", table, err)
	}
	return out, nil
}

// vacuumAdmission withholds on a visible vacuum of the target; a vacuum
// whose table is hidden proceeds as progress_unknown; none proceeds.
func vacuumAdmission(rows []runningVacuum) (string, *runningVacuum) {
	var unknown *runningVacuum
	for i := range rows {
		if rows[i].RelIDKnown {
			return reasonVacuumInProgress, &rows[i]
		}
		if unknown == nil {
			unknown = &rows[i]
		}
	}
	if unknown != nil {
		return vacuumProgressUnknown, unknown
	}
	return "", nil
}

func (v runningVacuum) detail(table string) string {
	who := "a manual VACUUM"
	if v.Autovacuum {
		who = "autovacuum"
	}
	if v.Wraparound {
		who += " (to prevent wraparound)"
	}
	return fmt.Sprintf("%s pid %d is vacuuming %s: phase %q, %d/%d heap blocks scanned; "+
		"retry after it finishes", who, v.PID, table, v.Phase, v.HeapBlksScanned,
		v.HeapBlksTotal)
}

func (v runningVacuum) evidence() map[string]any {
	return map[string]any{"pid": v.PID, "phase": v.Phase,
		"heap_blks_scanned": v.HeapBlksScanned, "heap_blks_total": v.HeapBlksTotal,
		"is_wraparound": v.Wraparound, "autovacuum": v.Autovacuum}
}

// admitVacuumProgress is the vacuum_table admission after the privilege
// precheck. A probe that cannot be read proceeds: the VACUUM's own
// lock_timeout still bounds a wait behind another vacuum.
func (e *Executor) admitVacuumProgress(
	ctx context.Context, table string, decision ActionPolicyDecision,
) error {
	readCtx, cancel := context.WithTimeout(ctx, maintenancePrivilegeTimeout)
	defer cancel()
	rows, err := readRunningVacuums(readCtx, catalogread.New(e.pool, catalogread.Default()),
		table)
	if err != nil {
		e.logFn("executor", "vacuum progress admission of %s unread, proceeding: %v",
			table, err)
		return nil
	}
	verdict, running := vacuumAdmission(rows)
	switch verdict {
	case reasonVacuumInProgress:
		e.recordVacuumProgress(ctx, decision.DecisionID, true, running.evidence())
		return e.withheldVacuum(decision, running.detail(table))
	case vacuumProgressUnknown:
		e.recordVacuumProgress(ctx, decision.DecisionID, false,
			map[string]any{"status": vacuumProgressUnknown, "pid": running.PID})
	}
	return nil
}

func (e *Executor) withheldVacuum(decision ActionPolicyDecision, detail string) error {
	decision.Decision, decision.BlockedReason, decision.Detail =
		PolicyDecisionBlocked, reasonVacuumInProgress, detail
	return &causedWithheldError{WithheldError: &WithheldError{Decision: decision},
		cause: ErrVacuumInProgress}
}

// recordVacuumProgress adds the admission's evidence to the authorizing
// decision and, when withheld, turns it into the block it became.
func (e *Executor) recordVacuumProgress(
	ctx context.Context, decisionID int64, withheld bool, evidence map[string]any,
) {
	if e.pool == nil || decisionID <= 0 {
		return
	}
	if _, err := e.pool.Exec(ctx, `/* pg_sage executor */ UPDATE sage.decision
		SET verdict = CASE WHEN $2 THEN 'blocked' ELSE verdict END,
		    reason = CASE WHEN $2 THEN $3 ELSE reason END,
		    evidence = evidence || jsonb_build_object('vacuum_progress', $4::jsonb)
		WHERE id = $1 AND verdict = 'execute'`, decisionID, withheld,
		reasonVacuumInProgress, evidence); err != nil {
		e.logFn("executor", "record vacuum progress admission on decision %d (%v): %v",
			decisionID, evidence, err)
	}
}
