package agentdb

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

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

func (s *Store) ReconcileLiveProvisioning(
	ctx context.Context,
	registry *RunnerRegistry,
) (LifecycleReconcileResult, error) {
	if err := s.Ensure(ctx); err != nil {
		return LifecycleReconcileResult{}, err
	}
	lockConn, releaseLock, err := acquireLiveReconcileLock(ctx, s.pool)
	if err != nil {
		return LifecycleReconcileResult{}, err
	}
	defer releaseLock()
	var locked bool
	if err := lockConn.QueryRow(ctx,
		`/* pg_sage */ SELECT pg_try_advisory_lock(hashtext('agentdb-live-reconcile'))`,
	).Scan(&locked); err != nil {
		return LifecycleReconcileResult{}, err
	}
	if !locked {
		return LifecycleReconcileResult{}, ErrRateLimited
	}
	rows, err := lockConn.Query(ctx, selectDeploymentsSQL+`
		WHERE provisioning_level='instance'
			AND provider <> $1
			AND provisioning_status IN (
				'provisioning', 'destroy_pending', 'destroying', 'status_unknown'
			)`,
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
		if dep.Status != "deleted" && dep.TeardownOperationID != "" &&
			(dep.ProvisioningStatus == "destroy_pending" ||
				dep.ProvisioningStatus == "destroying" ||
				dep.ProvisioningStatus == "status_unknown") {
			attempt, destroyErr := s.destroyAuthorizedLive(ctx, dep, runner)
			if destroyErr != nil {
				result.Blocked = append(result.Blocked, LifecycleBlocked{
					DeploymentID: dep.DeploymentID,
					Reason:       destroyErr.Error(),
				})
				continue
			}
			result.DestroyLive = append(result.DestroyLive, attempt)
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

func acquireLiveReconcileLock(
	ctx context.Context,
	pool *pgxpool.Pool,
) (*pgxpool.Conn, func(), error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	release := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var unlocked bool
		err := conn.QueryRow(cleanupCtx,
			`/* pg_sage */ SELECT pg_advisory_unlock(`+
				`hashtext('agentdb-live-reconcile'))`,
		).Scan(&unlocked)
		if err == nil && unlocked {
			conn.Release()
			return
		}
		raw := conn.Hijack()
		_ = raw.Close(cleanupCtx)
	}
	return conn, release, nil
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
