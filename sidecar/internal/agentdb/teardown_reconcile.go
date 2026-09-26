package agentdb

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// maxTeardownRetriesPerPass bounds the archived-row revisit sweep; rows
// that stay blocked rotate to the back via teardown_blocked_at.
const maxTeardownRetriesPerPass = 100

const claimArchivedLiveSQL = `/* pg_sage */
	WITH pending AS (
		SELECT deployment_id FROM sage.agent_db_deployments
		WHERE status='archived' AND live_mode AND provider_resource_id <> ''
			AND provisioning_level='instance' AND provider <> 'local_postgres'
			AND provisioning_status IN ('available', 'status_checked')
			AND teardown_operation_id=''
			AND lease_expires_at IS NOT NULL AND lease_expires_at < $1
			AND (provider_mutation_id='' OR provider_mutation_expires_at <= now())
			AND NOT (deployment_id = ANY($3::text[]))
		ORDER BY teardown_blocked_at NULLS FIRST, lease_expires_at, deployment_id
		LIMIT $4
		FOR UPDATE SKIP LOCKED
	)
	UPDATE sage.agent_db_deployments AS deployment
	SET cleanup_claim_id=$2 || ':' || deployment.deployment_id,
		cleanup_claimed_at=now(),
		lifecycle_version=deployment.lifecycle_version+1, updated_at=now()
	WHERE deployment.deployment_id IN (SELECT deployment_id FROM pending)
	RETURNING ` + deploymentColumnsSQL

// ReconcileAbandonedDeployments archives newly expired leases and then
// revisits every archived deployment that still owns a live provider
// resource, until it is destroyed or blocked with a persisted reason.
// A failure on one deployment never drops the rest of the batch.
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
	seen := make([]string, 0, len(archived))
	for _, dep := range archived {
		seen = append(seen, dep.DeploymentID)
		s.reconcileExpiredDeployment(ctx, now, runnerSource, dep, &result)
	}
	pending, err := s.claimArchivedLiveTeardowns(ctx, now, seen)
	if err != nil {
		return result, err
	}
	for _, dep := range pending {
		s.reconcileExpiredDeployment(ctx, now, runnerSource, dep, &result)
	}
	return result, nil
}

func (s *Store) claimArchivedLiveTeardowns(
	ctx context.Context,
	now time.Time,
	exclude []string,
) ([]Deployment, error) {
	claimPrefix, err := lifecycleOperationID("cleanup")
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, claimArchivedLiveSQL,
		now, claimPrefix, exclude, maxTeardownRetriesPerPass)
	if err != nil {
		return nil, fmt.Errorf("claim archived live teardowns: %w", err)
	}
	defer rows.Close()
	return scanDeployments(rows)
}

func (s *Store) reconcileExpiredDeployment(
	ctx context.Context,
	now time.Time,
	runnerSource any,
	dep Deployment,
	result *LifecycleReconcileResult,
) {
	if dep.Provider == ProviderLocalPostgres || dep.ProvisioningLevel != LevelInstance {
		return
	}
	if dep.LiveMode {
		s.reconcileLiveTeardown(ctx, now, runnerSource, dep, result)
		return
	}
	s.reconcileDryRunTeardown(ctx, now, runnerSource, dep, result)
}

func (s *Store) reconcileLiveTeardown(
	ctx context.Context,
	now time.Time,
	runnerSource any,
	dep Deployment,
	result *LifecycleReconcileResult,
) {
	liveRunner, ok := liveRunnerFromSource(runnerSource, dep.Provider)
	if !ok {
		s.blockTeardown(ctx, result, dep.DeploymentID, "live runner unavailable")
		return
	}
	if err := s.mutationAllowed(ctx); err != nil {
		s.blockTeardown(ctx, result, dep.DeploymentID, teardownBlockReason(err))
		return
	}
	if err := s.requireOwnedLiveResource(ctx, dep); err != nil {
		s.blockTeardown(ctx, result, dep.DeploymentID, teardownBlockReason(err))
		return
	}
	authorized, err := s.authorizeTeardownClaim(ctx, dep, now)
	if err != nil {
		s.blockTeardown(ctx, result, dep.DeploymentID, teardownBlockReason(err))
		return
	}
	attempt, err := s.destroyAuthorizedLive(ctx, authorized, liveRunner)
	if err != nil {
		s.blockTeardown(ctx, result, dep.DeploymentID,
			"provider destroy will be retried: "+teardownBlockReason(err))
		return
	}
	result.DestroyLive = append(result.DestroyLive, attempt)
}

// reconcileDryRunTeardown plans a destroy for a deployment that never
// created a provider resource; it owns nothing, so no live call is made.
func (s *Store) reconcileDryRunTeardown(
	ctx context.Context,
	now time.Time,
	runnerSource any,
	dep Deployment,
	result *LifecycleReconcileResult,
) {
	runner, err := commandRunnerFromSource(runnerSource, dep.Provider)
	if err != nil {
		s.blockTeardown(ctx, result, dep.DeploymentID, "provision runner unavailable")
		return
	}
	authorized, err := s.authorizeTeardownClaim(ctx, dep, now)
	if err != nil {
		s.blockTeardown(ctx, result, dep.DeploymentID, teardownBlockReason(err))
		return
	}
	attempt, err := s.DestroyProvisionDryRun(ctx, authorized.DeploymentID, runner)
	if err != nil {
		s.blockTeardown(ctx, result, dep.DeploymentID, teardownBlockReason(err))
		return
	}
	result.DestroyDryRun = append(result.DestroyDryRun, attempt)
}

func teardownBlockReason(err error) string {
	switch {
	case errors.Is(err, ErrEmergencyStop):
		return "emergency stop active"
	case errors.Is(err, ErrRestoreRequired):
		return "verified restore required"
	case errors.Is(err, ErrNotOwned):
		return "no recorded live provider resource"
	case errors.Is(err, ErrInvalid):
		return "invalid provisioning plan or provider state"
	case errors.Is(err, ErrConflict):
		return "cleanup claim invalidated"
	case errors.Is(err, ErrRateLimited):
		return "provider mutation in progress or rate limited"
	default:
		return err.Error()
	}
}

// blockTeardown reports the block in the pass result and persists it on the
// row so operators can see why a live resource is still running.
func (s *Store) blockTeardown(
	ctx context.Context,
	result *LifecycleReconcileResult,
	id string,
	reason string,
) {
	result.Blocked = append(result.Blocked, LifecycleBlocked{
		DeploymentID: id, Reason: reason,
	})
	if _, err := s.pool.Exec(ctx, `/* pg_sage */
		UPDATE sage.agent_db_deployments
		SET teardown_blocked_reason=$2, teardown_blocked_at=now()
		WHERE deployment_id=$1`, id, reason); err != nil {
		result.Blocked = append(result.Blocked, LifecycleBlocked{
			DeploymentID: id, Reason: "persist block reason: " + err.Error(),
		})
	}
}
