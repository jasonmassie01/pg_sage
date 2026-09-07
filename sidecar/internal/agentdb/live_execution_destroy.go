package agentdb

import (
	"context"
)

func (s *Store) ValidateLiveDestroyPrerequisites(
	ctx context.Context,
	dep Deployment,
	policy LiveProvisionPolicy,
) error {
	if !policy.RequireBackupBeforeDrop {
		return nil
	}
	backups, err := s.Backups(ctx, dep.DeploymentID)
	if err != nil {
		return err
	}
	if !hasRestoreVerifiedBackup(backups) {
		return ErrRestoreRequired
	}
	return nil
}

func (s *Store) ExecuteDestroyProvisionLive(
	ctx context.Context,
	id string,
	runner ProviderRunner,
	req LiveExecutionRequest,
) (ProvisionAttempt, error) {
	dep, trusted, err := s.validateDestroyLiveRequest(ctx, id, runner, req)
	if err != nil {
		return ProvisionAttempt{}, err
	}
	if err := s.ValidateLiveDestroyPrerequisites(
		ctx, dep, trusted.CurrentPolicy,
	); err != nil {
		return ProvisionAttempt{}, err
	}
	if err := s.ClaimLiveExecution(ctx, *req.Attempt, req.Now); err != nil {
		return ProvisionAttempt{}, err
	}
	prepared, err := s.prepareDirectTeardown(ctx, dep)
	if err != nil {
		return ProvisionAttempt{}, err
	}
	attempt, err := s.runProviderDestroy(
		ctx, id, runner, prepared.TeardownOperationID,
	)
	if err != nil {
		return attempt, err
	}
	receipt := LiveExecutionReceipt{
		AuthorizationID:    req.Attempt.AuthorizationID,
		IdempotencyKey:     req.Attempt.IdempotencyKey,
		PlanHash:           req.Attempt.PlanHash,
		ProviderResourceID: dep.ProviderResourceID,
	}
	if err := s.PersistLiveExecutionReceipt(ctx, receipt); err != nil {
		return ProvisionAttempt{}, err
	}
	return attempt, nil
}

func (s *Store) validateDestroyLiveRequest(
	ctx context.Context,
	id string,
	runner ProviderRunner,
	req LiveExecutionRequest,
) (Deployment, LiveExecutionRecords, error) {
	dep, err := s.cloudDeploymentForExecution(ctx, id)
	if err != nil {
		return Deployment{}, LiveExecutionRecords{}, err
	}
	if runner == nil || runner.Name() == "dry_run" ||
		req.Records == nil || req.Attempt == nil ||
		req.Attempt.Operation != ProvisionOpDestroy {
		return Deployment{}, LiveExecutionRecords{}, ErrInvalid
	}
	trusted, err := s.LoadLiveExecutionRecords(ctx, *req.Attempt, *req.Records)
	if err != nil {
		return Deployment{}, LiveExecutionRecords{}, err
	}
	validation := ValidateLiveExecutionAttempt(trusted, *req.Attempt, req.Now)
	if validation.Replay || consumedLiveRecords(trusted) {
		return Deployment{}, LiveExecutionRecords{}, ErrConflict
	}
	if !validation.Allowed {
		return Deployment{}, LiveExecutionRecords{}, ErrInvalid
	}
	return dep, trusted, nil
}
