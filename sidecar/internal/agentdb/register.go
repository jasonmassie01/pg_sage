package agentdb

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

func (s *Store) Register(ctx context.Context, req RegisterRequest) (Deployment, error) {
	if err := s.Ensure(ctx); err != nil {
		return Deployment{}, err
	}
	if err := normalizeRegister(&req, s.opts.RequireBackupBeforeDestroy); err != nil {
		return Deployment{}, err
	}
	var dep Deployment
	err := scanDeployment(s.pool.QueryRow(ctx, registerSQL,
		req.DeploymentID,
		req.TenantID,
		req.AgentID,
		req.RunID,
		req.DatabaseName,
		req.SafetyMode,
		req.IsolationType,
		req.SchemaName,
		req.Provider,
		req.ProvisioningLevel,
		req.SizeProfileID,
		req.ProvisioningStatus,
		req.ProviderResourceID,
		req.SecretRef,
		req.SecretRefProvider,
		req.SecretRefExpiresAt,
		req.LiveMode,
		req.BudgetUSD,
		req.BackupRequired,
		req.LeaseSeconds,
		jsonBytes(req.Metadata),
		jsonBytes(req.ProvisioningPlan),
		jsonBytes(req.ConnectionInfo),
	), &dep)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.existingRegistration(ctx, req)
	}
	if err != nil {
		return Deployment{}, err
	}
	_ = s.audit(ctx, req.DeploymentID, "register", nil)
	_ = s.seedTuningHints(ctx, req.DeploymentID, req.Metadata)
	return dep, nil
}

// existingRegistration resolves a register that collided with a row the
// upsert refused to overwrite (live resource, other owner, in-flight state).
// The same owner and shape is an idempotent replay; anything else conflicts.
func (s *Store) existingRegistration(
	ctx context.Context,
	req RegisterRequest,
) (Deployment, error) {
	current, err := s.Get(ctx, req.DeploymentID)
	if err != nil {
		return Deployment{}, err
	}
	if current.TenantID != req.TenantID || current.AgentID != req.AgentID ||
		current.Provider != req.Provider ||
		current.ProvisioningLevel != req.ProvisioningLevel ||
		current.Status == "deleted" {
		return Deployment{}, ErrConflict
	}
	return current, nil
}

// normalizeRegister validates and defaults a registration. backup_required
// is forced on while agentdb.require_backup_before_destroy is set, and
// otherwise honours the request (G8-B12).
func normalizeRegister(req *RegisterRequest, requireBackup bool) error {
	req.DeploymentID = strings.TrimSpace(req.DeploymentID)
	req.TenantID = strings.TrimSpace(req.TenantID)
	req.AgentID = strings.TrimSpace(req.AgentID)
	if req.DeploymentID == "" || req.TenantID == "" || req.AgentID == "" {
		return ErrInvalid
	}
	normalizeProviderFields(req)
	if !validProvider(req.Provider) || !validLevel(req.ProvisioningLevel) {
		return ErrInvalid
	}
	if err := ValidateSecretRef(req.SecretRef); err != nil {
		return err
	}
	if cloudProvider(req.Provider) && req.ProvisioningLevel != LevelInstance {
		return ErrInvalid
	}
	if req.Provider == ProviderLocalPostgres && req.ProvisioningLevel == LevelInstance {
		return ErrInvalid
	}
	if req.SafetyMode == "" {
		req.SafetyMode = "observation"
	}
	if req.ProvisioningStatus == "" {
		req.ProvisioningStatus = "registered"
	}
	if req.LeaseSeconds <= 0 {
		req.LeaseSeconds = 3600
	}
	if req.Metadata == nil {
		req.Metadata = map[string]any{}
	}
	if req.ProvisioningPlan == nil {
		req.ProvisioningPlan = map[string]any{}
	}
	if req.ConnectionInfo == nil {
		req.ConnectionInfo = map[string]any{}
	}
	req.BackupRequired = req.BackupRequired || requireBackup
	return nil
}

// extendLeaseSQL renews a lease and releases any pending cleanup claim; it
// never touches a deployment whose teardown has started.
const extendLeaseSQL = `/* pg_sage */
		UPDATE sage.agent_db_deployments
		SET lease_expires_at=now()+make_interval(secs => $2),
			status=CASE
				WHEN status='archived' AND cleanup_claim_id <> '' THEN 'active'
				ELSE status
			END,
			cleanup_claim_id='',
			cleanup_claimed_at=NULL,
			teardown_operation_id='',
			provider_mutation_id='',
			provider_mutation_expires_at=NULL,
			lifecycle_version=lifecycle_version+1,
			updated_at=now()
		WHERE deployment_id=$1
			AND status <> 'deleted'
			AND provisioning_status NOT IN (
				'destroy_pending', 'destroying', 'destroyed'
			)
		RETURNING ` + deploymentColumnsSQL
