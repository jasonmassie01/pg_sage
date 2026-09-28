package executor

import (
	"context"
	"errors"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
)

// parkLeaseConflict parks an action whose DDL lease overlaps another writer
// (serialize_mode park). A park is not a failure: it writes no action_log
// row, so it neither counts toward the retry/abandon limit nor uses the
// self-initiated rate budget. The finding is retried next cycle. The park
// is recorded in the decision ledger with reason ddl_conflict. It returns
// false for nil and for any other lease error.
func (e *Executor) parkLeaseConflict(
	ctx context.Context, f analyzer.Finding, decisionID int64, err error,
) bool {
	if !errors.Is(err, policy.ErrLeaseConflict) {
		return false
	}
	e.logFn("executor", "parked %q: DDL lease held by another writer (%s); "+
		"retrying next cycle", f.Title, policy.ReasonDDLConflict)
	if e.pool == nil || decisionID <= 0 {
		return true
	}
	if _, recordErr := e.pool.Exec(ctx, recordParkSQL, decisionID,
		string(policy.ReasonDDLConflict), ledger.NewEvidenceID()); recordErr != nil {
		e.logFn("executor", "record park decision for %d: %v", decisionID, recordErr)
	}
	return true
}

// recordParkSQL records the park as a new ledger decision derived from the
// execute decision it replaces, linked through evidence.parked_decision_id.
const recordParkSQL = `/* pg_sage */
INSERT INTO sage.decision
    (database_id, feature, intent, target_objects, policy_id, policy_version,
     verdict, risk_tier, reason, guardrails, evidence, evidence_id)
SELECT database_id, feature, intent, target_objects, policy_id, policy_version,
       'parked', risk_tier, $2, guardrails,
       evidence || jsonb_build_object('parked_decision_id', id), $3
  FROM sage.decision WHERE id = $1`
