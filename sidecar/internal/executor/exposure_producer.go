package executor

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/policy"
)

// The exposure producer (fleetpol-02b). Every cycle it reads the open
// AP-03 and AP-15 findings and, when the policy document's exposure mode
// is enforce, turns their would-be actions into typed remediations through
// the one execution pipeline: the standing gate decides (RLS always needs
// approval), Apply holds the table's change lease, and the action is
// recorded, verified and reversible. In shadow mode it proposes nothing and
// withdraws the exposure proposals still pending.

// exposureWithdrawnReason is the queue reason of a withdrawn proposal.
const exposureWithdrawnReason = "exposure.mode is shadow: exposure proposal withdrawn"

// exposureFindingsSQL reads the open exposure findings this producer acts
// on (AP-15 per schema, AP-03 per table).
const exposureFindingsSQL = `/* pg_sage executor */ SELECT id, category, object_type,
  object_identifier, title, COALESCE(detail, '{}'::jsonb)
FROM sage.findings
WHERE status = 'open' AND ((category = $1 AND object_type = 'schema')
   OR (category = $2 AND object_type = 'table'))
ORDER BY id
LIMIT 200`

type exposureFindingRow struct {
	id int64
	f  analyzer.Finding
}

// runExposureRemediations is the producer's cycle step. An unknown mode
// (the policy unreadable) neither proposes nor withdraws.
func (e *Executor) runExposureRemediations(ctx context.Context, isReplica bool) {
	enforced, known := e.exposureEnforced(ctx)
	if !known {
		return
	}
	if !enforced {
		e.withdrawExposureProposals(ctx)
		return
	}
	if isReplica || e.effectiveExecMode() == "manual" || !e.ExecutorEnabled() {
		return
	}
	rows, err := e.openExposureFindings(ctx)
	if err != nil {
		e.logFn("executor", "read exposure findings: %v", err)
		return
	}
	for _, row := range rows {
		for _, r := range exposureRemediations(row.f) {
			e.processExposure(ctx, r, row.id, isReplica)
		}
	}
}

// exposureEnforced reads the standing document's exposure mode.
func (e *Executor) exposureEnforced(ctx context.Context) (enforced, known bool) {
	e.policyMu.RLock()
	read := e.exposureMode
	e.policyMu.RUnlock()
	if read == nil {
		return false, true // no standing document: shadow
	}
	mode, err := read(ctx)
	if err != nil {
		e.logFn("executor", "read exposure mode: %v", err)
		return false, false
	}
	return mode == policy.ExposureModeEnforce, true
}

func (e *Executor) openExposureFindings(ctx context.Context) ([]exposureFindingRow, error) {
	if e.pool == nil {
		return nil, nil
	}
	rows, err := e.pool.Query(ctx, exposureFindingsSQL, exposureCategoryPublicGrants,
		exposureCategoryRLS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []exposureFindingRow
	for rows.Next() {
		var row exposureFindingRow
		var detail []byte
		if err := rows.Scan(&row.id, &row.f.Category, &row.f.ObjectType,
			&row.f.ObjectIdentifier, &row.f.Title, &detail); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(detail, &row.f.Detail); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// processExposure gates one remediation: nothing to change ends quietly,
// a target pg_sage cannot change ends the proposal once with the reason,
// and the gate's verdict queues or runs it through Apply.
func (e *Executor) processExposure(
	ctx context.Context, f analyzer.Finding, findingID int64, isReplica bool,
) {
	stmt, ok := parseExposureSQL(f.RecommendedSQL)
	gate := e.StandingPolicyGate()
	if !ok || gate == nil || e.actionStore == nil ||
		e.pendingApproval(ctx, f, findingID) || e.exposureRefusedBefore(ctx, f, findingID) ||
		e.exceedsMaxRetries(ctx, findingID) {
		return
	}
	_, err := e.precheckExposure(ctx, stmt)
	switch {
	case errors.Is(err, ErrExposureNoChange), errors.Is(err, ErrNotExposureTarget):
		return // a view in AP-15's list, or already remediated
	case errors.Is(err, ErrExposureNotOwner):
		e.logRefusedAction(ctx, f, findingID, map[string]any{}, 0, err)
		return
	case err != nil:
		e.logFn("executor", "exposure precheck of %s: %v", stmt.qualified(), err)
		return
	}
	request := findingRequest(f, isReplica)
	if stmt.kind == exposureEnableRLS {
		if request.Evidence == nil {
			request.Evidence = map[string]any{}
		}
		request.Evidence["exposure_card"] = e.rlsCard(ctx, stmt)
	}
	decision := standingPolicyDecision(gate.Authorize(ctx, request))
	switch decision.Decision {
	case PolicyDecisionExecute:
	case PolicyDecisionQueueApproval:
		e.queueFinding(ctx, f, findingID, decision, nil)
		return
	default:
		return
	}
	e.applyExposure(ctx, f, stmt, findingID, request, decision)
}

// applyExposure runs an authorized remediation through Apply, which takes
// the table's change lease and re-authorizes after its waits.
func (e *Executor) applyExposure(
	ctx context.Context, f analyzer.Finding, stmt exposureStmt, findingID int64,
	request policy.ActionRequest, decision ActionPolicyDecision,
) {
	lease := f
	_, err := e.Apply(ctx, ActionIntent{
		Request: request, Lease: &lease, FirstDecision: &decision,
		Execute: func(ctx context.Context, d ActionPolicyDecision) (int64, error) {
			return e.runAuthorizedExposure(ctx, f, stmt, findingID, d), nil
		},
		Refused: func(ctx context.Context, decisionID int64, err error) {
			e.logFn("executor", "exposure lease denied for %s: %v", stmt.qualified(), err)
		},
	})
	if err != nil {
		e.logFn("executor", "exposure remediation %s withheld: %v", stmt.qualified(), err)
	}
}

// runAuthorizedExposure executes, records and starts verification. A
// statement that found nothing to change records no action.
func (e *Executor) runAuthorizedExposure(
	ctx context.Context, f analyzer.Finding, stmt exposureStmt, findingID int64,
	decision ActionPolicyDecision,
) int64 {
	beforeState := e.snapshotBeforeState(ctx, nil)
	var readers map[int64]int64
	if stmt.kind == exposureRevoke {
		readers = e.revokeReaderCalls(ctx, stmt)
	}
	err := e.execExposure(ctx, &f, stmt, beforeState, e.lockTimeoutMS(f.RecommendedSQL,
		decision))
	if errors.Is(err, ErrExposureNoChange) {
		e.releaseBudget(ctx, decision.DecisionID)
		return 0
	}
	actionID := e.logActionWithDecision(ctx, f, findingID, beforeState, decision.DecisionID,
		err)
	if err != nil {
		e.recordFindingFailure(ctx, f, actionID, err)
		return actionID
	}
	e.recordExposureRemediation(ctx, actionID, beforeState)
	e.finishFinding(ctx, f, actionID)
	if stmt.kind == exposureRevoke {
		e.startRevokeWatch(ctx, f, actionID, stmt, readers)
	} else {
		e.monitorFinding(ctx, f, actionID)
	}
	return actionID
}

// exposureRefusedBefore reports an ownership refusal already recorded for
// this statement and finding: it is permanent, so it is never retried.
func (e *Executor) exposureRefusedBefore(
	ctx context.Context, f analyzer.Finding, findingID int64,
) bool {
	var refused bool
	err := e.pool.QueryRow(ctx, `/* pg_sage executor */ SELECT EXISTS (
		SELECT 1 FROM sage.action_log WHERE finding_id = $1 AND sql_executed = $2
		  AND outcome = 'failed' AND rollback_reason LIKE $3)`,
		findingID, f.RecommendedSQL, "%"+ErrExposureNotOwner.Error()+"%").Scan(&refused)
	if err != nil {
		e.logFn("executor", "check exposure refusals: %v", err)
		return true // unknown: never retry blind
	}
	return refused
}

// withdrawExposureProposals supersedes every pending exposure proposal of
// this database's findings; executed actions keep their rollback.
func (e *Executor) withdrawExposureProposals(ctx context.Context) {
	if e.pool == nil {
		return
	}
	tag, err := e.pool.Exec(ctx, `/* pg_sage executor */ UPDATE sage.action_queue
		SET status = 'superseded', reason = $1
		WHERE status = 'pending' AND action_type IN ($2, $3)
		  AND finding_id IN (SELECT id FROM sage.findings WHERE category IN ($4, $5))`,
		exposureWithdrawnReason, ActionTypeRevokePublicGrant, ActionTypeEnableRLS,
		exposureCategoryPublicGrants, exposureCategoryRLS)
	if err != nil {
		e.logFn("executor", "withdraw exposure proposals: %v", err)
		return
	}
	if n := tag.RowsAffected(); n > 0 {
		e.logFn("executor", "withdrew %d exposure proposal(s): exposure.mode is shadow", n)
	}
}
