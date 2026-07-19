package agentdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const claimExpiredSQL = `/* pg_sage */
	WITH expired AS (
		SELECT deployment_id FROM sage.agent_db_deployments
		WHERE status IN ('active', 'budget_exceeded')
			AND lease_expires_at IS NOT NULL AND lease_expires_at < $1
		ORDER BY lease_expires_at, deployment_id
		FOR UPDATE SKIP LOCKED
	)
	UPDATE sage.agent_db_deployments AS deployment
	SET status='archived', cleanup_claim_id=$2 || ':' || deployment.deployment_id,
		cleanup_claimed_at=now(), teardown_operation_id='',
		provider_mutation_id='', provider_mutation_expires_at=NULL,
		lifecycle_version=deployment.lifecycle_version+1, updated_at=now()
	WHERE deployment.deployment_id IN (SELECT deployment_id FROM expired)
	RETURNING ` + deploymentColumnsSQL

const authorizeTeardownSQL = `/* pg_sage */
	UPDATE sage.agent_db_deployments
	SET provisioning_status='destroy_pending', teardown_operation_id=$4,
		provider_mutation_id='', provider_mutation_expires_at=NULL,
		lifecycle_version=lifecycle_version+1, updated_at=now()
	WHERE deployment_id=$1 AND status='archived' AND cleanup_claim_id=$2
		AND lifecycle_version=$3 AND lease_expires_at IS NOT NULL
		AND lease_expires_at < $5 AND safety_mode=$6 AND live_mode=$7
		AND backup_required=$8 AND provider=$9 AND provisioning_level=$10
		AND provisioning_status=$11
		AND (NOT backup_required OR EXISTS (
			SELECT 1 FROM sage.agent_db_backups backup
			WHERE backup.deployment_id=$1 AND backup.status='restore_verified'
				AND backup.restore_verified_at IS NOT NULL
		))
	RETURNING ` + deploymentColumnsSQL

func (s *Store) claimExpiredDeployments(
	ctx context.Context,
	now time.Time,
) ([]Deployment, error) {
	claimPrefix, err := lifecycleOperationID("cleanup")
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, claimExpiredSQL, now, claimPrefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	claimed, err := scanDeployments(rows)
	if err != nil {
		return nil, err
	}
	for _, dep := range claimed {
		_ = s.audit(ctx, dep.DeploymentID, "lease_expiry_claimed", map[string]any{
			"lifecycle_version": dep.LifecycleVersion,
		})
	}
	return claimed, nil
}

func (s *Store) authorizeTeardownClaim(
	ctx context.Context,
	claimed Deployment,
	now time.Time,
) (Deployment, error) {
	if claimed.CleanupClaimID == "" || claimed.LifecycleVersion <= 0 {
		return Deployment{}, ErrConflict
	}
	current, err := s.Get(ctx, claimed.DeploymentID)
	if err != nil {
		return Deployment{}, err
	}
	if !cleanupClaimMatches(current, claimed, now) {
		return Deployment{}, ErrConflict
	}
	if current.ProvisioningLevel == LevelInstance {
		if _, err := commandsFromPlan(current.ProvisioningPlan); err != nil {
			return Deployment{}, err
		}
	}
	if err := s.requireRestoreVerifiedBackup(ctx, current); err != nil {
		return Deployment{}, err
	}
	operationID, err := lifecycleOperationID("teardown")
	if err != nil {
		return Deployment{}, err
	}
	var authorized Deployment
	err = scanDeployment(s.pool.QueryRow(ctx, authorizeTeardownSQL,
		claimed.DeploymentID,
		claimed.CleanupClaimID,
		claimed.LifecycleVersion,
		operationID,
		now,
		claimed.SafetyMode,
		claimed.LiveMode,
		claimed.BackupRequired,
		claimed.Provider,
		claimed.ProvisioningLevel,
		claimed.ProvisioningStatus,
	), &authorized)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, getErr := s.Get(ctx, claimed.DeploymentID); errors.Is(getErr, ErrNotFound) {
			return Deployment{}, ErrNotFound
		}
		return Deployment{}, ErrConflict
	}
	if err != nil {
		return Deployment{}, err
	}
	_ = s.audit(ctx, claimed.DeploymentID, "teardown_authorized", map[string]any{
		"operation_id":  operationID,
		"claim_version": claimed.LifecycleVersion,
	})
	return authorized, nil
}

func cleanupClaimMatches(current, claimed Deployment, now time.Time) bool {
	return current.Status == "archived" &&
		current.CleanupClaimID == claimed.CleanupClaimID &&
		current.LifecycleVersion == claimed.LifecycleVersion &&
		leaseExpired(current, now) &&
		current.SafetyMode == claimed.SafetyMode &&
		current.LiveMode == claimed.LiveMode &&
		current.BackupRequired == claimed.BackupRequired &&
		current.Provider == claimed.Provider &&
		current.ProvisioningLevel == claimed.ProvisioningLevel &&
		current.ProvisioningStatus == claimed.ProvisioningStatus
}

func scanDeployments(rows pgx.Rows) ([]Deployment, error) {
	deployments := []Deployment{}
	for rows.Next() {
		var dep Deployment
		if err := scanDeployment(rows, &dep); err != nil {
			return nil, err
		}
		deployments = append(deployments, dep)
	}
	return deployments, rows.Err()
}

func lifecycleOperationID(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate %s operation id: %w", prefix, err)
	}
	return prefix + "_" + hex.EncodeToString(raw[:]), nil
}
