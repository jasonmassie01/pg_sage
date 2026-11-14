package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/policy"
)

// reasonCustodianBackoff is the decision reason of an exhausted backoff.
const reasonCustodianBackoff = "custodian_backoff"

// custodianEscalationTimeout bounds recording one escalation.
const custodianEscalationTimeout = 10 * time.Second

// escalateBackoff raises an exhausted backoff of a red proposal to an
// operator (vacuum-05): before, the full stop only became a parked or
// blocked verdict that alerted no one, so the more urgent the table, the
// more certain the silent failure. One escalation per failure streak.
func (e *Executor) escalateBackoff(ctx context.Context, proposal CustodianProposal, err error) {
	var backoff *custodianBackoffError
	if errors.As(err, &backoff) && backoff.exhausted {
		e.escalateCustodianBackoff(ctx, proposal, backoff)
	}
}

// escalateCustodianBackoff records one custodian_backoff decision and
// dispatches one critical custodian_escalation event the first time a red
// proposal's failures reach the maximum. A decision already recorded since
// the streak's newest failure means it was escalated: the check and the
// insert run under an advisory lock on the statement, so concurrent
// submitters escalate once.
func (e *Executor) escalateCustodianBackoff(
	ctx context.Context, proposal CustodianProposal, backoff *custodianBackoffError,
) {
	if e.pool == nil || !custodianRed(proposal) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), custodianEscalationTimeout)
	defer cancel()
	sum := sha256.Sum256([]byte(proposal.SQL))
	sha := hex.EncodeToString(sum[:])
	decisionID, err := e.recordCustodianEscalation(ctx, proposal, backoff, sha)
	if err != nil {
		e.logFn("ERROR", "custodian backoff of %q exhausted (%d failures) but its "+
			"escalation was not recorded: %v", proposal.SQL, backoff.failures, err)
		return
	}
	if decisionID == 0 {
		return
	}
	e.dispatchEvent(ctx, notify.CustodianEscalationEvent(notify.CustodianEscalation{
		Database: e.databaseName, Feature: proposal.Feature,
		Target: strings.Join(proposal.TargetObjects, ","), SQL: proposal.SQL,
		SQLSHA256: sha, Failures: backoff.failures,
		Window: custodianBackoffWindow.String(), DecisionID: decisionID,
		Profile: custodianProfile(proposal)}))
}

// custodianProfile is the proposal's VACUUM profile evidence (vacuum-05b:
// "emergency"), empty when it has none. An emergency statement differs
// from the normal one, so its failure streak escalates on its own.
func custodianProfile(p CustodianProposal) string {
	profile, _ := p.Evidence["profile"].(string)
	return profile
}

// custodianRed reports a proposal that answers a red horizon: a critical
// deadline, or a freeze (the freeze custodian only freezes red tables).
func custodianRed(p CustodianProposal) bool {
	if p.Deadline != nil && p.Deadline.Urgency == policy.UrgencyCritical {
		return true
	}
	return p.Feature == "freeze"
}

// recordCustodianEscalation inserts the custodian_backoff decision unless
// this streak already has one; it returns 0 when it inserted nothing.
func (e *Executor) recordCustodianEscalation(
	ctx context.Context, proposal CustodianProposal, backoff *custodianBackoffError,
	sha string,
) (int64, error) {
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin custodian escalation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `/* pg_sage executor */ SELECT pg_advisory_xact_lock(
		hashtextextended('custodian_backoff:' || $1, 0))`, sha); err != nil {
		return 0, fmt.Errorf("lock custodian escalation: %w", err)
	}
	var escalated bool
	if err := tx.QueryRow(ctx, `/* pg_sage executor */ SELECT EXISTS (SELECT 1 FROM sage.decision
		WHERE reason = $1 AND evidence->>'sql_sha256' = $2 AND created_at >= $3)`,
		reasonCustodianBackoff, sha, backoff.last).Scan(&escalated); err != nil {
		return 0, fmt.Errorf("read custodian escalation: %w", err)
	}
	if escalated {
		return 0, nil
	}
	id, err := ledger.NewTxRepository(tx).InsertDecision(ctx,
		e.escalationDecision(proposal, backoff, sha))
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit custodian escalation: %w", err)
	}
	return id, nil
}

func (e *Executor) escalationDecision(
	proposal CustodianProposal, backoff *custodianBackoffError, sha string,
) ledger.DecisionInput {
	riskTier := "high"
	if contract, ok := contractForCustodianProposal(proposal.SQL); ok &&
		contract.BaseRiskTier != "" {
		riskTier = contract.BaseRiskTier
	}
	input := ledger.DecisionInput{DatabaseID: e.databaseID,
		Feature: custodianPolicyFeature(proposal), Intent: proposal.Feature,
		Evidence: map[string]any{"sql_sha256": sha, "failures": backoff.failures,
			"last_failure_at": backoff.last.UTC().Format(time.RFC3339Nano),
			"window":          custodianBackoffWindow.String()},
		ProposedSQL: proposal.SQL, Verdict: ledger.VerdictBlocked,
		Reason: reasonCustodianBackoff, RiskTier: riskTier,
		TargetObjects: append([]string(nil), proposal.TargetObjects...),
		EvidenceID:    ledger.NewEvidenceID()}
	if profile := custodianProfile(proposal); profile != "" {
		input.Evidence["profile"] = profile
	}
	if proposal.Deadline != nil && !proposal.Deadline.HardAt.IsZero() {
		hardAt := proposal.Deadline.HardAt
		input.DeadlineKind, input.DeadlineHardAt = string(proposal.Deadline.Kind), &hardAt
	}
	return input
}
