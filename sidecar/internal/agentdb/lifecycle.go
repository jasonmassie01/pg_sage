package agentdb

import (
	"context"
	"errors"
	"time"
)

func (s *Store) ReconcileAbandonedDeployments(
	ctx context.Context,
	now time.Time,
	runner ProvisionRunner,
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
		return LifecycleReconcileResult{}, err
	}
	return result, nil
}
