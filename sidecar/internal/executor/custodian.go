package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/policy"
)

var ErrCustodianProposalWithheld = errors.New("custodian proposal withheld by policy")

type CustodianProposal struct {
	Feature       string
	SQL           string
	TargetObjects []string
	IsReplica     bool
	Deadline      *policy.DeadlineContext
	Evidence      map[string]any
}

func (e *Executor) SubmitVerifiedIndexProposal(
	ctx context.Context, proposal CustodianProposal,
	rollbackSQL string, queryIDs []int64,
) error {
	decision := e.EvaluateCustodianProposal(ctx, proposal)
	if decision.Decision != PolicyDecisionExecute {
		return fmt.Errorf("%w: %s", ErrCustodianProposalWithheld, decision.BlockedReason)
	}
	finding := custodianFinding(proposal)
	finding.RollbackSQL = rollbackSQL
	finding.Detail = map[string]any{"queryids": append([]int64(nil), queryIDs...)}
	if _, err := verifiedActionForFinding(finding); err != nil {
		return fmt.Errorf("route custodian index through verification: %w", err)
	}
	if e.indexVerification == nil {
		return ErrVerificationUnavailable
	}
	if err := e.indexVerification.Admit(ctx); err != nil {
		return err
	}
	e.executeFinding(ctx, finding, 0, decision.DecisionID)
	return nil
}

func (e *Executor) EvaluateCustodianProposal(
	ctx context.Context, proposal CustodianProposal,
) ActionPolicyDecision {
	e.policyMu.RLock()
	gate := e.policyGate
	e.policyMu.RUnlock()
	if gate == nil {
		return ActionPolicyDecision{
			Decision: PolicyDecisionBlocked, RiskTier: "unknown",
			BlockedReason: "standing policy gate is unavailable",
		}
	}
	request := policy.ActionRequest{
		SQL: proposal.SQL, Feature: custodianPolicyFeature(proposal),
		TargetObjs: append([]string(nil), proposal.TargetObjects...),
		IsReplica:  proposal.IsReplica,
		Deadline:   proposal.Deadline,
		Evidence:   cloneCustodianEvidence(proposal.Evidence),
	}
	if contract, ok := contractForCustodianProposal(proposal.SQL); ok {
		request.Contract = policyContract(contract)
	}
	return standingPolicyDecision(gate.Authorize(ctx, request))
}

func cloneCustodianEvidence(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func (e *Executor) SubmitCustodianProposal(
	ctx context.Context, proposal CustodianProposal,
) error {
	decision := e.EvaluateCustodianProposal(ctx, proposal)
	if decision.Decision != PolicyDecisionExecute {
		return fmt.Errorf("%w: %s", ErrCustodianProposalWithheld, decision.BlockedReason)
	}
	if e.pool == nil {
		return fmt.Errorf("execute custodian proposal: database pool unavailable")
	}
	finding := custodianFinding(proposal)
	releaseLease, err := e.acquireDDLLease(ctx, finding, decision.DecisionID)
	if err != nil {
		return fmt.Errorf("acquire custodian change lease: %w", err)
	}
	defer releaseLease()
	decision = e.EvaluateCustodianProposal(ctx, proposal)
	if decision.Decision != PolicyDecisionExecute {
		return fmt.Errorf("%w after lease: %s",
			ErrCustodianProposalWithheld, decision.BlockedReason)
	}
	baseline, err := e.captureCustodianBaseline(ctx, proposal)
	if err != nil {
		return fmt.Errorf("capture custodian verification baseline: %w", err)
	}
	managedResult, managed, execErr := e.applyManagedCustodianConfig(ctx, proposal)
	if !managed {
		execErr = e.executeCustodianSQL(ctx, proposal.SQL)
	}
	actionID := e.logActionWithDecision(
		ctx, finding, 0, e.snapshotBeforeState(ctx, nil), decision.DecisionID, execErr,
	)
	if execErr != nil {
		return fmt.Errorf("execute custodian proposal: %w", execErr)
	}
	if managed {
		updateActionOutcome(ctx, e.pool, actionID,
			outcomeStatus(managedResult.InEffect), managedResult.Note)
	} else if isAlterSystem(proposal.SQL) {
		outcome := applyConfigChange(
			ctx, e.pool, proposal.SQL, e.cfg.CloudEnvironment, e.logFn,
		)
		updateActionOutcome(ctx, e.pool, actionID,
			outcomeStatus(outcome.InEffect), outcome.Note)
	}
	criterion, verifyErr := "custodian_wal", error(nil)
	if !managed {
		criterion, verifyErr = e.verifyCustodianAction(ctx, proposal, baseline)
	}
	if verifyErr != nil {
		verificationID, _ := finalizeActionVerification(
			ctx, e.pool, actionID, "unverifiable", verifyErr.Error())
		setCustodianVerificationCriterion(ctx, e.pool, verificationID, criterion)
		updateActionOutcome(ctx, e.pool, actionID, "failed",
			"custodian post-check failed: "+verifyErr.Error())
		return fmt.Errorf("verify custodian proposal: %w", verifyErr)
	}
	if actionID > 0 {
		updateActionSuccess(ctx, e.pool, actionID)
		setActionCustodianCriterion(ctx, e.pool, actionID, criterion)
	}
	return nil
}

type custodianBaseline struct{ freezeAge *int64 }

func (e *Executor) captureCustodianBaseline(
	ctx context.Context, proposal CustodianProposal,
) (custodianBaseline, error) {
	if proposal.Feature != "freeze" || len(proposal.TargetObjects) != 1 {
		return custodianBaseline{}, nil
	}
	age, err := e.freezeAge(ctx, proposal.TargetObjects[0])
	if err != nil {
		return custodianBaseline{}, err
	}
	return custodianBaseline{freezeAge: &age}, nil
}

func (e *Executor) verifyCustodianAction(
	ctx context.Context, proposal CustodianProposal, baseline custodianBaseline,
) (string, error) {
	if proposal.Feature == "freeze_blocker" {
		return e.verifyXminBlocker(ctx, proposal.Evidence)
	}
	if proposal.Feature == "freeze" && baseline.freezeAge != nil {
		after, err := e.freezeAge(ctx, proposal.TargetObjects[0])
		if err != nil || after > *baseline.freezeAge {
			return "custodian_freeze", fmt.Errorf("freeze horizon did not improve")
		}
		return "custodian_freeze", nil
	}
	if proposal.Feature == "autovacuum_tuning" && len(proposal.TargetObjects) == 1 {
		return e.verifyAutovacuumTuning(ctx, proposal.TargetObjects[0])
	}
	if isAlterSystem(proposal.SQL) {
		var setting int64
		err := e.pool.QueryRow(ctx, `SELECT setting::bigint FROM pg_settings
			WHERE name='max_slot_wal_keep_size'`).Scan(&setting)
		if err != nil || setting < 0 {
			return "custodian_wal", fmt.Errorf("WAL backstop is not effective")
		}
		return "custodian_wal", nil
	}
	if isDDLMutation(proposal.SQL) && len(proposal.TargetObjects) == 1 {
		var exists bool
		err := e.pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL",
			proposal.TargetObjects[0]).Scan(&exists)
		if err != nil || !exists {
			return "custodian_schema", fmt.Errorf("schema target post-check failed")
		}
		return "custodian_schema", nil
	}
	return "custodian_unknown", fmt.Errorf("no custodian post-check is available")
}

func (e *Executor) verifyXminBlocker(
	ctx context.Context, evidence map[string]any,
) (string, error) {
	pid, ok := evidence["pid"].(int)
	if !ok || pid <= 0 {
		return "custodian_xmin_blocker", fmt.Errorf("blocker PID evidence is missing")
	}
	var cleared bool
	err := e.pool.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM pg_stat_activity
		WHERE pid=$1 AND backend_xmin IS NOT NULL)`, pid).Scan(&cleared)
	if err != nil || !cleared {
		return "custodian_xmin_blocker", fmt.Errorf("xmin blocker remains active")
	}
	return "custodian_xmin_blocker", nil
}

func (e *Executor) verifyAutovacuumTuning(
	ctx context.Context, target string,
) (string, error) {
	var effective bool
	err := e.pool.QueryRow(ctx, `SELECT COALESCE(reloptions,'{}') @>
		ARRAY['autovacuum_vacuum_scale_factor=0.02']
		FROM pg_class WHERE oid=to_regclass($1)`, target).Scan(&effective)
	if err != nil || !effective {
		return "custodian_autovacuum", fmt.Errorf("autovacuum tuning is not effective")
	}
	return "custodian_autovacuum", nil
}

func (e *Executor) freezeAge(ctx context.Context, target string) (int64, error) {
	var age int64
	err := e.pool.QueryRow(ctx, `SELECT age(relfrozenxid)::bigint FROM pg_class
		WHERE oid=to_regclass($1)`, target).Scan(&age)
	return age, err
}

func setActionCustodianCriterion(
	ctx context.Context, pool *pgxpool.Pool, actionID int64, criterion string,
) {
	_, _ = pool.Exec(ctx, `UPDATE sage.verification
		SET criterion=jsonb_build_object('kind',$2::text), updated_at=now()
		WHERE action_log_id=$1`,
		actionID, criterion)
}

func setCustodianVerificationCriterion(
	ctx context.Context, pool *pgxpool.Pool, verificationID int64, criterion string,
) {
	if verificationID <= 0 {
		return
	}
	_, _ = pool.Exec(ctx, `UPDATE sage.verification
		SET criterion=jsonb_build_object('kind',$2), updated_at=now() WHERE id=$1`,
		verificationID, criterion)
}

func (e *Executor) executeCustodianSQL(ctx context.Context, sql string) error {
	if err := ValidateExecutorSQL(sql); err != nil {
		return err
	}
	timeout := e.cfg.Safety.DDLTimeout()
	var err error
	if NeedsConcurrently(sql) || NeedsTopLevel(sql) {
		err = ExecConcurrently(ctx, e.pool, sql, timeout,
			WithLockTimeout(e.cfg.Safety.LockTimeout()))
	} else {
		err = ExecInTransaction(ctx, e.pool, sql, timeout)
	}
	if err == nil {
		e.notifyPostDDL(ctx, sql)
	}
	return err
}

func contractForCustodianProposal(sql string) (ActionContract, bool) {
	if err := ValidateExecutorSQL(sql); err != nil {
		return ActionContract{}, false
	}
	return ContractForActionType(actionTypeForProposalSQL(sql))
}

func custodianPolicyFeature(proposal CustodianProposal) string {
	if proposal.Feature == "freeze_blocker" {
		return string(policy.ChangeFreeze)
	}
	if proposal.Feature == "wal" && isAlterSystem(proposal.SQL) {
		return string(policy.ChangeConfigGUC)
	}
	return proposal.Feature
}

func custodianFinding(proposal CustodianProposal) analyzer.Finding {
	target := strings.Join(proposal.TargetObjects, ",")
	return analyzer.Finding{
		Category: proposal.Feature, ObjectType: "custodian",
		ObjectIdentifier: target, Title: proposal.Feature + " custodian action",
		RecommendedSQL: proposal.SQL, ActionRisk: "safe",
	}
}
