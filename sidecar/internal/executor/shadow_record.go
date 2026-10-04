package executor

import (
	"context"
	"errors"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/recommendation"
	"github.com/pg-sage/sidecar/internal/shadow"
)

// Shadow mode (roadmap 1.4): below a class's earned level, every action
// pg_sage would have taken is recorded as a shadow decision, once per
// fingerprint per window: the exact SQL, the rollback, the prediction (the
// same model as real actions, without a frozen verification baseline),
// the evidence and the gate's verdict had the class been trusted. It
// runs nothing and reads only catalogs, statistics and sage tables, so it
// never locks a user object. The approval queue is unchanged; the shadow
// is recorded beside it, so the operator's decision can score it.

// shadowWindow is the dedupe window of shadow decisions.
var shadowWindow = shadow.DefaultOptions().DedupeWindow

// GrantedLevelReader names the granted level of a request's ledger pair
// (earned.Limiter). Without it (the ledger is not enforced, or failed)
// nothing is shadowed: there is no earned level to be below.
type GrantedLevelReader interface {
	GrantedLevel(context.Context, policy.ActionRequest) (int, error)
}

// shadowable reports a verdict that withholds the action for want of
// trust: observe-only (trust level or ledger), an approval (ceiling or
// ledger handoff) or a disabled tier. Hard stops and parks are not.
func shadowable(d ActionPolicyDecision) bool {
	switch d.Decision {
	case PolicyDecisionObserveOnly, PolicyDecisionQueueApproval:
		return true
	case PolicyDecisionBlocked:
		return d.BlockedReason == string(policy.ReasonTrustRampNotSatisfied)
	}
	return false
}

// shadowWithheldCustodian records the shadow decision of a custodian
// proposal the gate withheld at its first authorization. Incident-family
// remediations earn from their own evidence (bench, reviews) and are not
// shadowed here.
func (e *Executor) shadowWithheldCustodian(ctx context.Context, proposal CustodianProposal,
	err error) {
	var withheld *WithheldError
	if !errors.As(err, &withheld) || withheld.Reauthorized {
		return
	}
	e.recordShadow(ctx, custodianRequest(proposal), custodianFinding(proposal), 0, nil,
		withheld.Decision)
}

// recordShadow records the shadow decision of f (authorized as req) when
// its self-initiated class is below L3. Failures are logged: shadow mode
// never changes what the cycle does.
func (e *Executor) recordShadow(ctx context.Context, req policy.ActionRequest,
	f analyzer.Finding, findingID int64, cand *recommendation.Candidate,
	decision ActionPolicyDecision) {
	reader, ok := e.autonomyLimiter().(GrantedLevelReader)
	if e.pool == nil || !ok || !shadowable(decision) {
		return
	}
	family, class := earned.FamilyForRequest(req)
	if req.Contract == nil || req.IncidentFamily != "" || !earned.IsSelfInitiated(family) {
		return
	}
	shape := shadow.Shape(f.RecommendedSQL)
	d := shadow.Decision{DatabaseID: e.databaseID, Database: e.databaseName,
		Fingerprint: shadow.Fingerprint(string(class), f.ObjectIdentifier, shape),
		Family:      string(family), Class: string(class), FindingID: findingID,
		Title: f.Title, Object: f.ObjectIdentifier, SQL: f.RecommendedSQL, Shape: shape}
	store := shadow.NewStore(e.pool)
	seen, err := store.Seen(ctx, e.databaseID, d.Fingerprint, shadowWindow)
	if err != nil || seen {
		e.logShadowError("check", f, err)
		return
	}
	if d.GrantedLevel, err = reader.GrantedLevel(ctx, req); err != nil ||
		d.GrantedLevel >= int(earned.L3) {
		e.logShadowError("read the level for", f, err)
		return
	}
	e.completeShadow(ctx, &d, f, decision, cand)
	if _, _, err := store.Record(ctx, d); err != nil {
		e.logShadowError("record", f, err)
	}
}

func (e *Executor) logShadowError(what string, f analyzer.Finding, err error) {
	if err != nil {
		e.logFn("executor", "shadow mode: %s %q: %v", what, f.Title, err)
	}
}

// completeShadow adds what pg_sage would have run with: the rollback
// (for a config change, the one that restores the captured prior value),
// the prediction, the evidence and the gate's verdicts.
func (e *Executor) completeShadow(ctx context.Context, d *shadow.Decision,
	f analyzer.Finding, decision ActionPolicyDecision, cand *recommendation.Candidate) {
	before := map[string]any{}
	d.RollbackSQL = f.RollbackSQL
	config, err := e.prepareConfigChange(ctx, f.RecommendedSQL)
	switch {
	case err != nil:
		e.logShadowError("capture the prior configuration of", f, err)
	case config != nil:
		d.RollbackSQL = config.rollbackSQL
		config.record(before)
	}
	d.Prediction = e.predictEffect(ctx, f.RecommendedSQL, f.Detail, before)
	d.Evidence = map[string]any{"finding_category": f.Category,
		"finding_severity": f.Severity, "finding_detail": f.Detail, "before_state": before,
		"policy_detail": decision.Detail, "evidence_id": decision.EvidenceID}
	d.GateVerdict, d.GateReason = gateVerdictOf(decision.Decision), decision.BlockedReason
	d.TrustedVerdict, d.TrustedReason = decision.TrustedVerdict, decision.TrustedReason
	d.TrustedDetail, d.DecisionID = decision.TrustedDetail, decision.DecisionID
	if d.TrustedVerdict == "" {
		// The gate decided before the ledger (the trust level or a tier
		// flag is the ceiling): trusted, the class would meet the same.
		d.TrustedVerdict, d.TrustedReason = d.GateVerdict, d.GateReason
	}
	if cand != nil {
		d.RecommendationID = cand.ID
	}
}

// gateVerdictOf is the gate verdict of an executor decision.
func gateVerdictOf(decision string) string {
	if decision == PolicyDecisionQueueApproval {
		return string(policy.VerdictQueueApproval)
	}
	return decision
}
