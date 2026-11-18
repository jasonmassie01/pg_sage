package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// The operator-approved path of an exposure remediation (fleetpol-02b): a
// queued revoke or RLS enable the operator approved runs through
// ExecuteManual, which re-authorizes it (shadow mode refuses it), checks
// the finding still names the table, and runs the typed statement.

// exposureManualFinding reads the open exposure finding an approved
// statement acts on. Unlike other findings it carries no recommended_sql:
// the statement must be one of the remediations the finding names. An
// AP-15 finding stays the target of its other relations after one revoke,
// so acted_on_at does not end it.
func (e *Executor) exposureManualFinding(
	ctx context.Context, findingID int, sql string,
) (manualFinding, analyzer.Finding, error) {
	var mf manualFinding
	f := analyzer.Finding{}
	err := e.pool.QueryRow(ctx, `/* pg_sage executor */ SELECT COALESCE(detail, '{}'::jsonb),
		  category, object_type, object_identifier, title
		FROM sage.findings WHERE id = $1 AND status = 'open' AND resolved_at IS NULL`,
		findingID).Scan(&mf.detail, &f.Category, &f.ObjectType, &f.ObjectIdentifier, &f.Title)
	if errors.Is(err, pgx.ErrNoRows) {
		return mf, f, ErrFindingNotActionable
	}
	if err != nil {
		return mf, f, fmt.Errorf("checking finding %d: %w", findingID, err)
	}
	if err := json.Unmarshal(mf.detail, &f.Detail); err != nil {
		return mf, f, fmt.Errorf("finding %d detail: %w", findingID, err)
	}
	mf.category, mf.ident = f.Category, f.ObjectIdentifier
	stmt, ok := parseExposureSQL(sql)
	if !ok {
		return mf, f, ErrFindingSQLMismatch
	}
	if err := exposureTargetRefusal(f, stmt); err != nil {
		return mf, f, err
	}
	f.RecommendedSQL = sql
	return mf, f, nil
}

// execExposureManual runs an approved exposure statement and records its
// exact rollback in beforeState (exposureRollbackSQL reads it back).
func (e *Executor) execExposureManual(
	ctx context.Context, findingID int, sql string, beforeState map[string]any,
	lockMS int,
) error {
	_, f, err := e.exposureManualFinding(ctx, findingID, sql)
	if err != nil {
		return err
	}
	stmt, _ := parseExposureSQL(sql)
	if stmt.kind == exposureRevoke {
		beforeState[exposureReadersKey] = e.revokeReaderCalls(ctx, stmt)
	}
	if err := e.execExposure(ctx, &f, stmt, beforeState, lockMS); err != nil {
		return err
	}
	if state, ok := beforeState[exposureStateKey].(map[string]any); ok {
		state["rollback_sql"] = f.RollbackSQL
	}
	return nil
}

// exposureReadersKey holds the revoke's reader calls until the action is
// recorded; it is removed before before_state is written.
const exposureReadersKey = "exposure_readers"

// exposureRollbackSQL is the exact rollback an executed exposure statement
// recorded, or "".
func exposureRollbackSQL(beforeState map[string]any) string {
	state, _ := beforeState[exposureStateKey].(map[string]any)
	rollback, _ := state["rollback_sql"].(string)
	return rollback
}

// takeExposureReaders removes and returns the reader calls a manual revoke
// captured.
func takeExposureReaders(beforeState map[string]any) map[int64]int64 {
	readers, _ := beforeState[exposureReadersKey].(map[int64]int64)
	delete(beforeState, exposureReadersKey)
	return readers
}

// watchExposureRevoke starts the reader verification of an approved
// revoke and reports whether it did.
func (r *manualRun) watchExposureRevoke(ctx context.Context, actionID int64) bool {
	stmt, ok := parseExposureSQL(r.sql)
	if !ok || stmt.kind != exposureRevoke {
		return false
	}
	f := analyzer.Finding{RecommendedSQL: r.sql, RollbackSQL: r.rollbackSQL,
		ObjectType: "table", ObjectIdentifier: stmt.qualified()}
	r.executor.startRevokeWatch(ctx, f, actionID, stmt, r.exposureReaders)
	return true
}
