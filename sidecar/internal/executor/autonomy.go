package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// Sage SRE M7: the earned-autonomy ledger restricts the standing gate for
// self-initiated family actions; an L2 verdict becomes a one-click
// approval handoff through the existing approval queue.

// handoffRejectCooldown keeps a handoff the operator rejected from being
// proposed again at once.
const handoffRejectCooldown = 24 * time.Hour

// handoffTTL is how long a handoff waits for a human.
const handoffTTL = 24 * time.Hour

// handoffCategory is the finding category that carries a handoff's SQL
// for the approval path (which executes approved items from a finding).
const handoffCategory = "autonomy_handoff"

// WithAutonomy installs the earned-autonomy ledger. Call it before
// EnableStandingPolicy*: the gate is built with it.
func (e *Executor) WithAutonomy(limiter policy.AutonomyLimiter) {
	e.policyMu.Lock()
	defer e.policyMu.Unlock()
	e.autonomy = limiter
}

func (e *Executor) autonomyLimiter() policy.AutonomyLimiter {
	e.policyMu.RLock()
	defer e.policyMu.RUnlock()
	return e.autonomy
}

// custodianIncidentFamily maps a custodian feature to the incident family
// it remediates: freezing and its blockers to the wraparound runway, WAL
// bounds to WAL retention. Other custodian work has no family.
func custodianIncidentFamily(feature string) string {
	switch feature {
	case "freeze", "freeze_blocker", "autovacuum_tuning":
		return string(earned.FamilyWraparound)
	case "wal":
		return string(earned.FamilyWAL)
	}
	return ""
}

// autonomyEvidence names the family and class of a family action in its
// decision record, so live outcomes can be attributed to the pair.
func autonomyEvidence(evidence map[string]any, request policy.ActionRequest) {
	if request.IncidentFamily == "" {
		return
	}
	evidence["incident_family"] = request.IncidentFamily
	evidence["autonomy_class"] = string(earned.ClassFor(request))
}

// handOffForApproval queues an L2 handoff for a withheld custodian
// proposal. It reports true when the proposal is handled: queued now,
// already waiting, or recently rejected by an operator.
func (e *Executor) handOffForApproval(
	ctx context.Context, proposal CustodianProposal, err error,
) bool {
	var withheld *WithheldError
	if !errors.As(err, &withheld) ||
		withheld.Decision.Decision != PolicyDecisionQueueApproval ||
		withheld.Decision.BlockedReason != string(policy.ReasonAutonomyHandoff) {
		return false
	}
	proposer, ok := e.actionStore.(ActionMetadataProposer)
	if !ok || e.pool == nil {
		e.logFn("executor", "autonomy handoff for %s withheld: no approval queue",
			proposal.Feature)
		return false
	}
	request := custodianRequest(proposal)
	key := earned.HandoffKey(earned.Family(request.IncidentFamily),
		earned.ClassFor(request), proposal.TargetObjects)
	if err := e.queueHandoff(ctx, proposer, proposal, request, key,
		withheld.Decision); err != nil {
		e.logFn("executor", "autonomy handoff %s failed: %v", key, err)
		return false
	}
	return true
}

func (e *Executor) queueHandoff(
	ctx context.Context, proposer ActionMetadataProposer, proposal CustodianProposal,
	request policy.ActionRequest, key string, decision ActionPolicyDecision,
) error {
	var busy bool
	if err := e.pool.QueryRow(ctx, `/* pg_sage */ SELECT EXISTS (
		SELECT 1 FROM sage.action_queue WHERE identity_key = $1
		  AND (status = 'pending' OR (status = 'rejected'
		       AND decided_at > now() - make_interval(secs => $2::double precision))))`,
		key, handoffRejectCooldown.Seconds()).Scan(&busy); err != nil {
		return fmt.Errorf("check pending handoff: %w", err)
	}
	if busy {
		return nil
	}
	findingID, err := e.handoffFinding(ctx, proposal, key)
	if err != nil {
		return err
	}
	expires := time.Now().UTC().Add(handoffTTL)
	meta := store.ActionProposalMetadata{IdentityKey: key,
		PolicyDecision: PolicyDecisionQueueApproval, Guardrails: decision.Guardrails,
		VerificationStatus: "not_started", ExpiresAt: &expires}
	if request.Contract != nil {
		meta.ActionType = request.Contract.ActionType
	}
	queueID, err := proposer.ProposeWithMetadata(ctx, e.databaseID, findingID,
		proposal.SQL, "", decision.RiskTier, meta)
	if err != nil {
		return fmt.Errorf("queue handoff: %w", err)
	}
	e.logFn("executor", "autonomy L2: handed %s to one-click approval", key)
	e.requestApproval(ctx, "earned L2 handoff "+key, proposal.SQL, decision.RiskTier,
		decision.DecisionID, queueID)
	return nil
}

// handoffFinding is the open finding that carries the handoff's SQL (the
// approval path executes an approved item from its finding), reused while
// it stays open.
func (e *Executor) handoffFinding(
	ctx context.Context, proposal CustodianProposal, key string,
) (int, error) {
	var id int
	err := e.pool.QueryRow(ctx, `/* pg_sage */ SELECT id FROM sage.findings
		WHERE category = $1 AND object_identifier = $2 AND status = 'open'
		  AND acted_on_at IS NULL AND recommended_sql = $3
		ORDER BY id DESC LIMIT 1`, handoffCategory, key, proposal.SQL).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("read handoff finding: %w", err)
	}
	detail, jsonErr := json.Marshal(map[string]any{"feature": proposal.Feature,
		"targets": proposal.TargetObjects, "evidence": proposal.Evidence})
	if jsonErr != nil {
		return 0, fmt.Errorf("encode handoff detail: %w", jsonErr)
	}
	err = e.pool.QueryRow(ctx, `/* pg_sage */ INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail,
		 recommendation, recommended_sql, status)
		VALUES ($1, 'warning', 'custodian', $2, $3, $4, $5, $6, 'open') RETURNING id`,
		handoffCategory, key, proposal.Feature+" remediation awaiting approval", detail,
		"pg_sage earned L2 for this action: approve to run it", proposal.SQL).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("record handoff finding: %w", err)
	}
	return id, nil
}

// OperatorBound is the operator's configured outer bound (executor on,
// execution mode, trust level, tier toggles), read without touching the
// database. The ledger carries over the autonomy it already grants.
func (e *Executor) OperatorBound() policy.RuntimeState {
	if e == nil {
		return policy.RuntimeState{}
	}
	cfg, mode, enabled := e.policySnapshot()
	bound := policy.RuntimeState{ExecutorEnabled: enabled, ExecutionMode: mode}
	if cfg != nil {
		bound.TrustLevel = cfg.Trust.Level
		bound.Tier3Safe, bound.Tier3Moderate = cfg.Trust.Tier3Safe, cfg.Trust.Tier3Moderate
	}
	return bound
}
