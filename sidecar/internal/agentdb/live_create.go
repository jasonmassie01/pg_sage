package agentdb

import (
	"context"
	"errors"
	"time"
)

// ExecuteProvisionLive runs an authorized live create. The create operation
// id is made durable (with live_mode) before the provider call, so a crash
// or an ambiguous provider error leaves a create_uncertain row that the
// reconciler resolves by lookup instead of a retryable 'failed' row that
// could create a second billed resource (G8-B06).
func (s *Store) ExecuteProvisionLive(
	ctx context.Context,
	id string,
	runner ProviderRunner,
	req LiveExecutionRequest,
) (ProvisionAttempt, error) {
	dep, commands, err := s.validateLiveCreate(ctx, id, runner, &req)
	if err != nil {
		return ProvisionAttempt{}, err
	}
	mutationID, err := s.beginProviderMutation(ctx, id)
	if err != nil {
		return ProvisionAttempt{}, err
	}
	defer s.releaseProviderMutation(id, mutationID)
	if err := s.ClaimLiveExecution(ctx, *req.Attempt, req.Now); err != nil {
		return ProvisionAttempt{}, err
	}
	operationID := "create:" + req.CostEstimateID
	if err := s.beginLiveCreate(ctx, id, operationID); err != nil {
		return ProvisionAttempt{}, err
	}
	result := runner.Create(ctx, ProvisionRequest{
		Operation: ProvisionOpCreate, OperationID: operationID,
		Deployment: dep, Plan: dep.ProvisioningPlan, Policy: req.Policy,
		RequestedAt: time.Now().UTC(), DryRunCommand: commands[0],
	})
	return s.finishLiveCreate(ctx, dep, runner, req, commands, result)
}

func (s *Store) validateLiveCreate(
	ctx context.Context,
	id string,
	runner ProviderRunner,
	req *LiveExecutionRequest,
) (Deployment, []ProviderCommand, error) {
	dep, err := s.cloudDeploymentForExecution(ctx, id)
	if err != nil {
		return Deployment{}, nil, err
	}
	if runner == nil || runner.Name() == "dry_run" || req.Records == nil || req.Attempt == nil {
		return Deployment{}, nil, ErrInvalid
	}
	if err := s.mutationAllowed(ctx); err != nil {
		return Deployment{}, nil, err
	}
	trusted, err := s.LoadLiveExecutionRecords(ctx, *req.Attempt, *req.Records)
	if err != nil {
		return Deployment{}, nil, err
	}
	validation := ValidateLiveExecutionAttempt(trusted, *req.Attempt, req.Now)
	if validation.Replay || consumedLiveRecords(trusted) {
		return Deployment{}, nil, ErrConflict
	}
	if !validation.Allowed || trusted.Estimate == nil {
		return Deployment{}, nil, ErrInvalid
	}
	req.CostEstimateID = trusted.Estimate.EstimateID
	req.Policy = trusted.CurrentPolicy
	if err := liveCreateStateAllowed(dep); err != nil {
		return Deployment{}, nil, err
	}
	commands, err := commandsFromPlan(dep.ProvisioningPlan)
	return dep, commands, err
}

func liveCreateStateAllowed(dep Deployment) error {
	if dep.ProviderMutationID != "" && dep.ProviderMutationExpiresAt != nil &&
		dep.ProviderMutationExpiresAt.After(time.Now().UTC()) {
		return ErrRateLimited
	}
	switch dep.ProvisioningStatus {
	case "available", "create_uncertain", "provisioning", "status_unknown":
		// Either live already or the outcome of an earlier create is unknown:
		// it must be reconciled, never blindly re-created.
		return ErrConflict
	case "preflight_passed", "failed", "dry_run_ready", "status_checked":
	default:
		return ErrInvalid
	}
	if dep.ProviderResourceID != "" || dep.CreateOperationID != "" {
		return ErrConflict
	}
	return nil
}

func (s *Store) beginLiveCreate(ctx context.Context, id, operationID string) error {
	if err := s.updateProvisioningStatus(ctx, id, "provisioning", nil); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `/* pg_sage */
		UPDATE sage.agent_db_deployments
		SET create_operation_id=$2, live_mode=true, updated_at=now()
		WHERE deployment_id=$1`, id, operationID)
	return err
}

func (s *Store) finishLiveCreate(
	ctx context.Context,
	dep Deployment,
	runner ProviderRunner,
	req LiveExecutionRequest,
	commands []ProviderCommand,
	result ProvisionResult,
) (ProvisionAttempt, error) {
	id := dep.DeploymentID
	nextStatus := liveCreateOutcome(&result)
	if result.Error == nil {
		if err := s.PersistLiveExecutionReceipt(ctx, LiveExecutionReceipt{
			AuthorizationID: req.Attempt.AuthorizationID,
			IdempotencyKey:  req.Attempt.IdempotencyKey, PlanHash: req.Attempt.PlanHash,
			ProviderResourceID: result.ProviderResourceID,
		}); err != nil {
			return ProvisionAttempt{}, err
		}
	}
	detail := RedactProviderDetail(result.Detail)
	detail["mode"], detail["cost_estimate_id"] = "live", req.CostEstimateID
	attempt, err := s.recordProvisionAttempt(ctx, id, provisionAttemptInput{
		Kind: "execute_live", Status: attemptStatus(result.Error), Runner: runner.Name(),
		Command: commands[0].Args, Detail: detail, FinishedAt: time.Now().UTC(),
	})
	if err != nil {
		return ProvisionAttempt{}, err
	}
	if err := s.recordLiveCreateOutcome(ctx, dep, nextStatus, result, req.CostEstimateID,
		detail); err != nil {
		return ProvisionAttempt{}, err
	}
	_ = s.audit(ctx, id, "provision_execute_live_"+attemptStatus(result.Error), detail)
	if result.Error != nil {
		return attempt, publicProviderError(result.Error)
	}
	return attempt, nil
}

// liveCreateOutcome maps a runner result onto the next provisioning status.
// Only an explicit definitive rejection becomes the retryable 'failed'.
func liveCreateOutcome(result *ProvisionResult) string {
	if result.Error == nil && result.ProviderResourceID == "" {
		result.Error = errors.New("provider returned no resource identity")
	}
	if result.Error == nil {
		return firstNonEmpty(result.Status, "available")
	}
	if result.Status == "failed" {
		return "failed"
	}
	return "create_uncertain"
}

func attemptStatus(err error) string {
	if err != nil {
		return "failed"
	}
	return "succeeded"
}

func (s *Store) recordLiveCreateOutcome(
	ctx context.Context,
	dep Deployment,
	nextStatus string,
	result ProvisionResult,
	requestHash string,
	detail map[string]any,
) error {
	if result.ProviderResourceID != "" && result.Error == nil {
		if err := s.RecordCreationReceipt(ctx, CreationReceipt{
			DeploymentID: dep.DeploymentID, Provider: dep.Provider,
			ProviderResourceID: result.ProviderResourceID, OperationMode: "live",
			RequestHash: requestHash, Detail: detail,
		}); err != nil {
			return err
		}
	}
	if err := s.applyProvisionResult(ctx, dep.DeploymentID, nextStatus, result,
		nextStatus != "failed"); err != nil {
		return err
	}
	if nextStatus == "failed" {
		return s.clearFailedLiveCreate(ctx, dep.DeploymentID)
	}
	return nil
}

// clearFailedLiveCreate resets a definitively rejected create: the provider
// created nothing, so the row owns nothing and may be retried.
func (s *Store) clearFailedLiveCreate(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `/* pg_sage */
		UPDATE sage.agent_db_deployments
		SET live_mode=false, create_operation_id='', updated_at=now()
		WHERE deployment_id=$1 AND provider_resource_id=''
			AND provisioning_status='failed'`, id)
	return err
}
