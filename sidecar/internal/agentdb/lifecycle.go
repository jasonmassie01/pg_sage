package agentdb

import (
	"context"
	"errors"
	"time"
)

func (s *Store) ReconcileAbandonedDeployments(
	ctx context.Context,
	now time.Time,
	runnerSource any,
) (LifecycleReconcileResult, error) {
	archived, err := s.ArchiveExpired(ctx, now)
	if err != nil {
		return LifecycleReconcileResult{}, err
	}
	result := LifecycleReconcileResult{Archived: archived}
	for _, dep := range archived {
		if dep.Provider == ProviderLocalPostgres || dep.ProvisioningLevel != LevelInstance {
			continue
		}
		if liveRunner, ok := liveRunnerFromSource(runnerSource, dep.Provider); ok &&
			dep.LiveMode &&
			destroyableProvisioningStatus(dep.ProvisioningStatus) {
			attempt, err := s.DestroyProvisionLive(ctx, dep.DeploymentID, liveRunner)
			if err == nil {
				result.DestroyLive = append(result.DestroyLive, attempt)
				continue
			}
			if errors.Is(err, ErrRestoreRequired) {
				result.Blocked = append(result.Blocked, LifecycleBlocked{
					DeploymentID: dep.DeploymentID,
					Reason:       "verified restore required",
				})
				continue
			}
			return LifecycleReconcileResult{}, err
		}
		runner, err := commandRunnerFromSource(runnerSource, dep.Provider)
		if err != nil {
			if errors.Is(err, ErrInvalid) {
				result.Blocked = append(result.Blocked, LifecycleBlocked{
					DeploymentID: dep.DeploymentID,
					Reason:       "provision runner unavailable",
				})
				continue
			}
			return LifecycleReconcileResult{}, err
		}
		attempt, err := s.DestroyProvisionDryRun(ctx, dep.DeploymentID, runner)
		if err == nil {
			result.DestroyDryRun = append(result.DestroyDryRun, attempt)
			continue
		}
		if errors.Is(err, ErrRestoreRequired) {
			result.Blocked = append(result.Blocked, LifecycleBlocked{
				DeploymentID: dep.DeploymentID,
				Reason:       "verified restore required",
			})
			continue
		}
		if errors.Is(err, ErrInvalid) {
			result.Blocked = append(result.Blocked, LifecycleBlocked{
				DeploymentID: dep.DeploymentID,
				Reason:       "invalid provisioning plan or provider state",
			})
			continue
		}
		return LifecycleReconcileResult{}, err
	}
	return result, nil
}

func liveRunnerFromSource(source any, provider string) (ProviderRunner, bool) {
	registry, ok := source.(*RunnerRegistry)
	if !ok || registry == nil {
		return nil, false
	}
	runner, err := registry.ForProvider(provider)
	if err != nil || runner == nil || runner.Name() == "dry_run" {
		return nil, false
	}
	return runner, true
}

func destroyableProvisioningStatus(status string) bool {
	return status == "available" ||
		status == "status_checked" ||
		status == "dry_run_ready"
}

func (s *Store) ReconcileLiveProvisioning(
	ctx context.Context,
	registry *RunnerRegistry,
) (LifecycleReconcileResult, error) {
	if err := s.Ensure(ctx); err != nil {
		return LifecycleReconcileResult{}, err
	}
	var locked bool
	if err := s.pool.QueryRow(ctx,
		`/* pg_sage */ SELECT pg_try_advisory_lock(hashtext('agentdb-live-reconcile'))`,
	).Scan(&locked); err != nil {
		return LifecycleReconcileResult{}, err
	}
	if !locked {
		return LifecycleReconcileResult{}, ErrRateLimited
	}
	defer func() {
		_, _ = s.pool.Exec(ctx,
			`/* pg_sage */ SELECT pg_advisory_unlock(hashtext('agentdb-live-reconcile'))`)
	}()
	rows, err := s.pool.Query(ctx, selectDeploymentsSQL+`
		WHERE provisioning_level='instance'
			AND provider <> $1
			AND provisioning_status IN ('provisioning', 'destroying', 'status_unknown')`,
		ProviderLocalPostgres,
	)
	if err != nil {
		return LifecycleReconcileResult{}, err
	}
	defer rows.Close()
	result := LifecycleReconcileResult{}
	for rows.Next() {
		var dep Deployment
		if err := scanDeployment(rows, &dep); err != nil {
			return LifecycleReconcileResult{}, err
		}
		runner, err := registry.ForProvider(dep.Provider)
		if err != nil || runner.Name() == "dry_run" {
			result.Blocked = append(result.Blocked, LifecycleBlocked{
				DeploymentID: dep.DeploymentID,
				Reason:       "live runner unavailable",
			})
			continue
		}
		status := runner.Status(ctx, ProvisionRequest{
			Operation:   ProvisionOpStatus,
			Deployment:  dep,
			Plan:        dep.ProvisioningPlan,
			RequestedAt: time.Now().UTC(),
		})
		if status.Error != nil {
			result.Blocked = append(result.Blocked, LifecycleBlocked{
				DeploymentID: dep.DeploymentID,
				Reason:       status.Error.Error(),
			})
			continue
		}
		attempt, err := s.recordProvisionAttempt(ctx, dep.DeploymentID, provisionAttemptInput{
			Kind:       "live_reconcile_status",
			Status:     "succeeded",
			Runner:     runner.Name(),
			Detail:     RedactProviderDetail(status.Detail),
			FinishedAt: time.Now().UTC(),
		})
		if err != nil {
			return LifecycleReconcileResult{}, err
		}
		result.DestroyDryRun = append(result.DestroyDryRun, attempt)
		if err := s.applyProvisionResult(
			ctx, dep.DeploymentID, status.Status, status, false,
		); err != nil {
			result.Blocked = append(result.Blocked, LifecycleBlocked{
				DeploymentID: dep.DeploymentID,
				Reason:       err.Error(),
			})
			continue
		}
	}
	return result, rows.Err()
}

func commandRunnerFromSource(source any, provider string) (ProvisionRunner, error) {
	switch typed := source.(type) {
	case nil:
		return DryRunProvisionRunner{}, nil
	case ProvisionRunner:
		return typed, nil
	case *RunnerRegistry:
		return typed.CommandRunnerForProvider(provider)
	default:
		return nil, ErrInvalid
	}
}
