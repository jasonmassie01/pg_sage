package executor

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
)

// parkLeaseConflict parks an action whose change lease overlaps another
// writer (serialize_mode park, or a lease queue wait that ended without
// the lease). A park is not a failure: it writes no action_log row, so it
// neither counts toward the retry/abandon limit nor uses the
// self-initiated rate budget. The finding is retried next cycle. The park
// is recorded in the decision ledger with reason ddl_conflict. It returns
// false for nil and for any other lease error.
func (e *Executor) parkLeaseConflict(
	ctx context.Context, f analyzer.Finding, decisionID int64, err error,
) bool {
	if !isLeaseConflict(err) {
		return false
	}
	e.logFn("executor", "parked %q: change lease held by another writer (%s); "+
		"retrying next cycle: %v", f.Title, policy.ReasonDDLConflict, err)
	e.recordLeaseVerdict(ctx, decisionID, leaseVerdictParked, err)
	return true
}

func isLeaseConflict(err error) bool {
	return errors.Is(err, policy.ErrLeaseConflict) || errors.Is(err, policy.ErrLeaseBusy)
}

// leaseVerdict is how a lease conflict is recorded: a parked self-initiated
// action, or a refused (blocked) operator action.
type leaseVerdict struct {
	verdict string
	linkKey string
}

var (
	leaseVerdictParked  = leaseVerdict{verdict: "parked", linkKey: "parked_decision_id"}
	leaseVerdictRefused = leaseVerdict{verdict: "blocked", linkKey: "refused_decision_id"}
)

// recordLeaseVerdict records the conflict as a new ledger decision derived
// from the execute decision it replaces, with the lease holder and queue
// outcome as evidence. Best effort, with the lease wait bound: when the
// pool is exhausted the action is still parked or refused.
func (e *Executor) recordLeaseVerdict(
	ctx context.Context, decisionID int64, verdict leaseVerdict, err error,
) {
	if e.pool == nil || decisionID <= 0 {
		return
	}
	evidence, marshalErr := json.Marshal(leaseEvidence(err))
	if marshalErr != nil {
		e.logFn("executor", "encode lease conflict evidence for %d: %v", decisionID,
			marshalErr)
		evidence = []byte("{}")
	}
	recordCtx, cancel := context.WithTimeout(ctx, policy.LeaseConnectionWait)
	defer cancel()
	if _, recordErr := e.pool.Exec(recordCtx, recordLeaseVerdictSQL, decisionID,
		string(policy.ReasonDDLConflict), ledger.NewEvidenceID(), verdict.verdict,
		verdict.linkKey, evidence); recordErr != nil {
		e.logFn("executor", "record %s lease decision for %d: %v", verdict.verdict,
			decisionID, recordErr)
	}
}

// leaseEvidence names what the action waited for: the holder, when known,
// and how a lease queue wait ended.
func leaseEvidence(err error) map[string]any {
	evidence := map[string]any{}
	switch {
	case errors.Is(err, policy.ErrLeaseQueueTimeout):
		evidence["lease_queue"] = "timeout"
	case errors.Is(err, policy.ErrLeaseQueueFull):
		evidence["lease_queue"] = "full"
	case errors.Is(err, policy.ErrLeaseQueueDuplicate):
		evidence["lease_queue"] = "duplicate"
	}
	if errors.Is(err, policy.ErrLeaseBusy) {
		evidence["lease_busy"] = true
	}
	var conflict *policy.LeaseConflictError
	if errors.As(err, &conflict) && conflict.Holder != "" {
		evidence["lease_holder"] = conflict.Holder
		evidence["lease_holder_decision_id"] = conflict.DecisionID
		evidence["lease_object"] = conflict.Object
	}
	return evidence
}

// recordLeaseVerdictSQL records the conflict as a new ledger decision
// derived from the execute decision it replaces, linked through
// evidence.<parked|refused>_decision_id.
const recordLeaseVerdictSQL = `/* pg_sage */
INSERT INTO sage.decision
    (database_id, feature, intent, target_objects, policy_id, policy_version,
     verdict, risk_tier, reason, guardrails, evidence, evidence_id)
SELECT database_id, feature, intent, target_objects, policy_id, policy_version,
       $4, risk_tier, $2, guardrails,
       evidence || jsonb_build_object($5::text, id) || $6::jsonb, $3
  FROM sage.decision WHERE id = $1`
