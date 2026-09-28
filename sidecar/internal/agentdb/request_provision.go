package agentdb

import (
	"context"
	"log/slog"
	"strings"
)

// ProvisionApprovedRequest turns an approved request into exactly one
// deployment. The request is claimed atomically (approved, allowed by
// policy, unconsumed) and linked to the deployment id before anything is
// provisioned; a failed provision releases the claim so the approval is
// not lost, and a racing or repeated provision gets ErrConflict.
func (s *Store) ProvisionApprovedRequest(
	ctx context.Context,
	id string,
	req RequestProvisionRequest,
) (Deployment, error) {
	agentReq, err := s.GetRequest(ctx, id)
	if err != nil {
		return Deployment{}, err
	}
	if agentReq.Status != "approved" || agentReq.PolicyDecision != "allow" {
		return Deployment{}, ErrInvalid
	}
	if agentReq.ConsumedDeploymentID != "" || !requestMatches(agentReq, req) {
		return Deployment{}, ErrConflict
	}
	actor := strings.TrimSpace(req.ActorID)
	if actor == "" {
		return Deployment{}, ErrInvalid
	}
	deploymentID := firstNonEmpty(req.DeploymentID, "dep_"+idFrom(id))
	if err := s.claimApprovedRequest(ctx, id, deploymentID, actor); err != nil {
		return Deployment{}, err
	}
	dep, err := s.Provision(ctx, registerFromRequest(agentReq, deploymentID, req))
	if err != nil {
		s.releaseApprovedRequest(ctx, id, deploymentID)
		return Deployment{}, err
	}
	s.auditOrWarn(ctx, dep.DeploymentID, "request_consumed", map[string]any{
		"request_id": id, "decided_by": agentReq.DecidedBy, "consumed_by": actor,
	})
	return dep, nil
}

// requestMatches checks the caller's optional expectations against the
// approved request, so one owner's approval cannot provision for another.
func requestMatches(agentReq Request, req RequestProvisionRequest) bool {
	level := firstNonEmpty(agentReq.IsolationType, LevelSchema)
	checks := []struct{ want, got string }{
		{agentReq.TenantID, req.TenantID},
		{agentReq.AgentID, req.AgentID},
		{normalizeProvider(agentReq.Provider), req.Provider},
		{normalizeProvisioningLevel(level), req.ProvisioningLevel},
	}
	for _, check := range checks {
		got := strings.TrimSpace(check.got)
		if got == "" {
			continue
		}
		if !strings.EqualFold(got, check.want) {
			return false
		}
	}
	return true
}

func registerFromRequest(
	agentReq Request,
	deploymentID string,
	req RequestProvisionRequest,
) RegisterRequest {
	return RegisterRequest{
		DeploymentID:      deploymentID,
		TenantID:          agentReq.TenantID,
		AgentID:           agentReq.AgentID,
		RunID:             agentReq.RunID,
		DatabaseName:      agentReq.DatabaseName,
		Provider:          agentReq.Provider,
		ProvisioningLevel: firstNonEmpty(agentReq.IsolationType, LevelSchema),
		SizeProfileID:     req.SizeProfileID,
		SchemaName:        req.SchemaName,
		SecretRef:         req.SecretRef,
		SecretRefProvider: req.SecretRefProvider,
		LeaseSeconds:      req.LeaseSeconds,
		BudgetUSD:         agentReq.BudgetUSD,
		BackupRequired:    agentReq.BackupRequired,
		Metadata: mergeMap(req.Metadata, map[string]any{
			"provider_params": req.ProviderParams,
			"request_id":      agentReq.RequestID,
			"purpose":         agentReq.Purpose,
		}),
	}
}

// claimApprovedRequest is the single-use gate: one UPDATE re-checks the
// approval and links the request to the deployment id. Exactly one caller
// can win; the deployment id must not already belong to another request.
func (s *Store) claimApprovedRequest(ctx context.Context, id, deploymentID, actor string) error {
	tag, err := s.pool.Exec(ctx, `/* pg_sage */
		UPDATE sage.agent_db_requests
		SET consumed_deployment_id=$2, consumed_by=$3, consumed_at=now(), updated_at=now()
		WHERE request_id=$1
			AND status='approved'
			AND policy_decision='allow'
			AND consumed_deployment_id=''
			AND NOT EXISTS (
				SELECT 1 FROM sage.agent_db_requests other
				WHERE other.consumed_deployment_id=$2 AND other.request_id<>$1
			)`, id, deploymentID, actor)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
}

// releaseApprovedRequest undoes this caller's claim after a failed
// provision. It uses a fresh context so a cancelled request still releases.
func (s *Store) releaseApprovedRequest(ctx context.Context, id, deploymentID string) {
	releaseCtx := context.WithoutCancel(ctx)
	_, err := s.pool.Exec(releaseCtx, `/* pg_sage */
		UPDATE sage.agent_db_requests
		SET consumed_deployment_id='', consumed_by='', consumed_at=NULL, updated_at=now()
		WHERE request_id=$1 AND consumed_deployment_id=$2`, id, deploymentID)
	if err != nil {
		slog.Error("agentdb: releasing approved request claim failed",
			"request_id", id, "deployment_id", deploymentID, "error", err)
	}
}
