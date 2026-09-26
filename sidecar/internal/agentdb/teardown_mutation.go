package agentdb

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const teardownMutationLease = 15 * time.Minute

// prepareDirectTeardown makes a direct (operator-authorized) destroy durable.
// requireBackup comes from the effective live policy, not the row default,
// so require_backup_before_destroy=false is honoured (G8-B12).
func (s *Store) prepareDirectTeardown(
	ctx context.Context,
	dep Deployment,
	requireBackup bool,
) (Deployment, error) {
	if err := s.validateDirectTeardownState(ctx, dep); err != nil {
		return Deployment{}, err
	}
	if resumableTeardown(dep) {
		return dep, nil
	}
	operationID, err := lifecycleOperationID("teardown")
	if err != nil {
		return Deployment{}, err
	}
	var prepared Deployment
	err = scanDeployment(s.pool.QueryRow(ctx, `/* pg_sage */
		UPDATE sage.agent_db_deployments
		SET provisioning_status='destroy_pending', teardown_operation_id=$4,
			provider_mutation_id='', provider_mutation_expires_at=NULL,
			lifecycle_version=lifecycle_version+1, updated_at=now()
		WHERE deployment_id=$1 AND lifecycle_version=$2
			AND provisioning_status=$3 AND teardown_operation_id=''
			AND live_mode AND provider_resource_id <> ''
			AND (NOT $5 OR EXISTS (
				SELECT 1 FROM sage.agent_db_backups backup
				WHERE backup.deployment_id=$1 AND backup.status='restore_verified'
					AND backup.restore_verified_at IS NOT NULL
			))
		RETURNING `+deploymentColumnsSQL,
		dep.DeploymentID, dep.LifecycleVersion, dep.ProvisioningStatus, operationID,
		requireBackup,
	), &prepared)
	if errors.Is(err, pgx.ErrNoRows) {
		return Deployment{}, ErrConflict
	}
	return prepared, err
}

func (s *Store) beginProviderMutation(
	ctx context.Context,
	id string,
) (string, error) {
	mutationID, err := lifecycleOperationID("mutation")
	if err != nil {
		return "", err
	}
	err = s.pool.QueryRow(ctx, `/* pg_sage */
		UPDATE sage.agent_db_deployments
		SET provider_mutation_id=$2,
			provider_mutation_expires_at=now()+make_interval(secs => $3),
			lifecycle_version=lifecycle_version+1,
			updated_at=now()
		WHERE deployment_id=$1 AND status <> 'deleted'
			AND (
				provider_mutation_id='' OR provider_mutation_expires_at <= now()
			)
		RETURNING provider_mutation_id`,
		id,
		mutationID,
		int(teardownMutationLease.Seconds()),
	).Scan(&mutationID)
	if !errors.Is(err, pgx.ErrNoRows) {
		return mutationID, err
	}
	current, getErr := s.Get(ctx, id)
	if getErr != nil {
		return "", getErr
	}
	if current.Status == "deleted" {
		return "", ErrConflict
	}
	return "", ErrRateLimited
}

func (s *Store) releaseProviderMutation(id, mutationID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.pool.Exec(ctx, `/* pg_sage */
		UPDATE sage.agent_db_deployments
		SET provider_mutation_id='', provider_mutation_expires_at=NULL,
			updated_at=now()
		WHERE deployment_id=$1 AND provider_mutation_id=$2`, id, mutationID)
}
