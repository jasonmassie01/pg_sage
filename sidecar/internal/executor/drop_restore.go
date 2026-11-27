package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/policy"
)

// The soft-drop restore (executor-01). Every drop rule ships the dropped
// index's pg_get_indexdef() + ";" as its rollback. When the drop regresses,
// the monitor re-creates that definition CONCURRENTLY under the typed
// restore contract; the gate lets it pass a trust downgrade (it is a
// condition of the autonomy the drop ran under) but not a hard stop.

// ActionTypeRestoreDroppedIndex is the restore contract's action type.
const ActionTypeRestoreDroppedIndex = policy.ActionRestoreDroppedIndex

// Reasons recorded in action_log.rollback_reason, distinct from a policy
// refusal (standingWithheld).
const (
	rollbackSQLMismatch = "rollback_sql_mismatch"
	rollbackNoContract  = "no_rollback_contract"
)

// restoreSQLForDrop is the one form a soft-drop restore takes: the recorded
// definition without its trailing ";", CONCURRENTLY. It is idempotent, so a
// stored restore and a legacy raw definition normalize to the same SQL.
func restoreSQLForDrop(rollbackSQL string) string {
	sql := strings.TrimSpace(rollbackSQL)
	sql = strings.TrimSpace(strings.TrimSuffix(sql, ";"))
	return concurrentIndexRollbackSQL(sql)
}

// storedRollbackSQL is the rollback action_log records for sql: a drop's
// CONCURRENTLY restore (CHECK-02: every drop, manual ones too), any other
// rollback as given.
func storedRollbackSQL(sql, rollbackSQL string) string {
	if isDropActionSQL(sql) && strings.TrimSpace(rollbackSQL) != "" {
		return restoreSQLForDrop(rollbackSQL)
	}
	return rollbackSQL
}

// isDropActionSQL reports a forward index drop (drop_unused_index).
func isDropActionSQL(sql string) bool {
	return ActionTypeForSQL(sql) == "drop_unused_index"
}

// matchDropRestore accepts only restoreSQL equal (byte for byte) to the
// normalized recorded definition, a CREATE INDEX CONCURRENTLY of exactly
// the index the drop removed.
func matchDropRestore(restoreSQL, recordedRollback, droppedSQL string) error {
	recorded := restoreSQLForDrop(recordedRollback)
	switch {
	case !isDropActionSQL(droppedSQL):
		return fmt.Errorf("%s: action is not an index drop", rollbackSQLMismatch)
	case recorded == "" || restoreSQL != recorded:
		return fmt.Errorf("%s: the restore differs from the definition recorded with "+
			"the drop", rollbackSQLMismatch)
	case ActionTypeForSQL(recorded) != "create_index_concurrently":
		return fmt.Errorf("%s: the recorded definition is not a CREATE INDEX",
			rollbackSQLMismatch)
	}
	if err := rollbackTargetsCreatedIndex(recorded, droppedSQL); err != nil {
		return fmt.Errorf("%s: the restore re-creates another index than the drop "+
			"removed: %v", rollbackSQLMismatch, err)
	}
	return nil
}

// dropRecord is what the drop's action_log row holds.
type dropRecord struct {
	sql, rollbackSQL, outcome string
	findingID                 int64
}

func (e *Executor) loadDropRecord(ctx context.Context, actionID int64) (dropRecord, error) {
	var rec dropRecord
	if e.pool == nil {
		return rec, errors.New("no database to read the drop from")
	}
	err := e.pool.QueryRow(ctx, `/* pg_sage executor */ SELECT sql_executed,
		COALESCE(rollback_sql, ''), outcome, COALESCE(finding_id, 0)
		FROM sage.action_log WHERE id = $1`, actionID).
		Scan(&rec.sql, &rec.rollbackSQL, &rec.outcome, &rec.findingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return rec, fmt.Errorf("action %d not found", actionID)
	}
	if err != nil {
		return rec, fmt.Errorf("load drop action %d: %w", actionID, err)
	}
	return rec, nil
}

// authorizeDroppedIndexRestore authorizes the restore of action actionID's
// drop: the SQL must match the recorded definition, then the standing gate
// decides under the restore contract.
func (e *Executor) authorizeDroppedIndexRestore(
	ctx context.Context, actionID int64, restoreSQL string,
) RollbackVerdict {
	rec, err := e.loadDropRecord(ctx, actionID)
	if err != nil {
		return RollbackVerdict{Reason: "restore withheld: " + err.Error()}
	}
	if err := matchDropRestore(restoreSQL, rec.rollbackSQL, rec.sql); err != nil {
		return RollbackVerdict{Reason: err.Error()}
	}
	return e.gateDropRestore(ctx, restoreSQL)
}

// gateDropRestore asks the standing gate to run restoreSQL as the
// rollback of pg_sage's own drop, aimed at the index's table.
func (e *Executor) gateDropRestore(ctx context.Context, restoreSQL string) RollbackVerdict {
	gate := e.StandingPolicyGate()
	if gate == nil {
		return RollbackVerdict{Reason: standingWithheld + ": " + reasonNoStandingPolicy}
	}
	decision := standingPolicyDecision(gate.Authorize(ctx, dropRestoreRequest(restoreSQL)))
	// A restore re-creates the state before pg_sage's drop; like a
	// rollback, its authorization holds no budget slot.
	e.releaseBudget(ctx, decision.DecisionID)
	if decision.Decision == PolicyDecisionExecute {
		return RollbackVerdict{Allowed: true}
	}
	return RollbackVerdict{Reason: fmt.Sprintf("%s (%s: %s)", standingWithheld,
		decision.Decision, decision.BlockedReason)}
}

// dropRestoreRequest is the gate request of a restore.
func dropRestoreRequest(restoreSQL string) policy.ActionRequest {
	request := policy.ActionRequest{
		SQL: restoreSQL, Feature: string(policy.ChangeIndex),
		Contract: policyContract(restoreDroppedIndexContract()), Rollback: true,
	}
	if schema, table, _, ok := parseCreateIndexTarget(restoreSQL); ok {
		request.TargetObjs = []string{qualifiedTarget(schema, table)}
	}
	return request
}

func qualifiedTarget(schema, table string) string {
	if schema == "" {
		return table
	}
	return schema + "." + table
}

// withDropRestore makes cfg restore a regressed drop of action actionID:
// the restore authorizer decides, and an emergency stop holds the restore
// as an approval card.
func (e *Executor) withDropRestore(cfg *RollbackMonitorConfig, actionID int64) {
	cfg.Decide = func(ctx context.Context, sql string) RollbackVerdict {
		return e.authorizeDroppedIndexRestore(ctx, actionID, sql)
	}
	cfg.HoldForStop = e.holdDropRestore
}

// findingMonitorConfig is the monitor configuration of a background
// finding's action: a drop is restored under the restore contract, every
// other action is rolled back through the standing gate.
func (e *Executor) findingMonitorConfig(
	f analyzer.Finding, actionID int64,
) RollbackMonitorConfig {
	cfg := e.rollbackMonitorConfig(nil)
	if isDropActionSQL(f.RecommendedSQL) {
		e.withDropRestore(&cfg, actionID)
		return cfg
	}
	cfg.Decide = e.findingRollbackDecider(f, actionID)
	return cfg
}
