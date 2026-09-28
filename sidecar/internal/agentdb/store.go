package agentdb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type scanner interface {
	Scan(dest ...any) error
}

func (s *Store) CreateRequest(ctx context.Context, req RequestCreate) (Request, error) {
	if err := s.Ensure(ctx); err != nil {
		return Request{}, err
	}
	req.IsolationType = normalizeIsolation(req.IsolationType)
	req.Provider = normalizeProvider(req.Provider)
	if strings.TrimSpace(req.TenantID) == "" || strings.TrimSpace(req.AgentID) == "" {
		return Request{}, ErrInvalid
	}
	if req.Body == nil {
		req.Body = requestBody(req)
	}
	if req.IdempotencyKey != "" {
		old, err := s.requestByIdempotency(ctx, req.TenantID, req.IdempotencyKey)
		if err == nil {
			return sameRequest(old, req)
		}
		if !errors.Is(err, ErrNotFound) {
			return Request{}, err
		}
	}
	hash := bodyHash(req.Body)
	if req.RequestID == "" {
		req.RequestID = "req_" + hash[:16]
	}
	dec := DecideRequest(req)
	return s.insertRequest(ctx, req, hash, dec)
}

func (s *Store) ListRequests(ctx context.Context) ([]Request, error) {
	if err := s.Ensure(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, selectRequestsSQL+" ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Request{}
	for rows.Next() {
		var req Request
		if err := scanRequest(rows, &req); err != nil {
			return nil, err
		}
		out = append(out, req)
	}
	return out, rows.Err()
}

func (s *Store) GetRequest(ctx context.Context, id string) (Request, error) {
	if err := s.Ensure(ctx); err != nil {
		return Request{}, err
	}
	var req Request
	err := scanRequest(
		s.pool.QueryRow(ctx, selectRequestsSQL+" WHERE request_id=$1", id),
		&req,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Request{}, ErrNotFound
	}
	return req, err
}

func (s *Store) List(ctx context.Context) ([]Deployment, error) {
	if err := s.Ensure(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, selectDeploymentsSQL+`
		WHERE status <> 'deleted'
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Deployment{}
	for rows.Next() {
		var dep Deployment
		if err := scanDeployment(rows, &dep); err != nil {
			return nil, err
		}
		out = append(out, dep)
	}
	return out, rows.Err()
}

func (s *Store) Get(ctx context.Context, id string) (Deployment, error) {
	if err := s.Ensure(ctx); err != nil {
		return Deployment{}, err
	}
	var dep Deployment
	err := scanDeployment(
		s.pool.QueryRow(ctx, selectDeploymentsSQL+" WHERE deployment_id=$1", id),
		&dep,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	return dep, err
}

func (s *Store) Ping(ctx context.Context, id string, req PingRequest) (Deployment, error) {
	if err := s.Ensure(ctx); err != nil {
		return Deployment{}, err
	}
	health, err := normalizeAgentHealth(req.Status)
	if err != nil {
		return Deployment{}, err
	}
	if _, err := s.pool.Exec(ctx, `/* pg_sage */
		INSERT INTO sage.agent_db_pings(deployment_id, status, metrics)
		VALUES ($1, $2, $3::jsonb)`,
		id, health, jsonBytes(req.Metrics),
	); err != nil {
		return Deployment{}, err
	}
	// A heartbeat records liveness only. Lifecycle status, cleanup claims,
	// teardown state and budget enforcement are owned by operator and
	// reconciler paths and must never be reachable with a ping token.
	var dep Deployment
	err = scanDeployment(s.pool.QueryRow(ctx, `/* pg_sage */
		UPDATE sage.agent_db_deployments
		SET last_ping_at=now(), agent_status=$2
		WHERE deployment_id=$1
		RETURNING `+deploymentColumnsSQL, id, health), &dep)
	if errors.Is(err, pgx.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	return dep, err
}

// normalizeAgentHealth maps an agent-reported heartbeat status onto the
// closed agent health vocabulary. Lifecycle words are rejected.
func normalizeAgentHealth(status string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "active", "healthy", "ok":
		return "healthy", nil
	case "degraded":
		return "degraded", nil
	case "busy":
		return "busy", nil
	case "idle":
		return "idle", nil
	default:
		return "", ErrInvalid
	}
}

func (s *Store) ExtendLease(
	ctx context.Context,
	id string,
	req LeaseRequest,
) (Deployment, error) {
	if err := s.Ensure(ctx); err != nil {
		return Deployment{}, err
	}
	if req.LeaseSeconds <= 0 {
		return Deployment{}, ErrInvalid
	}
	current, err := s.Get(ctx, id)
	if err != nil {
		return Deployment{}, err
	}
	if err := leaseExtensionAllowed(current, req); err != nil {
		return Deployment{}, err
	}
	var dep Deployment
	err = scanDeployment(s.pool.QueryRow(ctx, extendLeaseSQL,
		id, req.LeaseSeconds,
	), &dep)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, getErr := s.Get(ctx, id); errors.Is(getErr, ErrNotFound) {
			return Deployment{}, ErrNotFound
		}
		return Deployment{}, ErrConflict
	}
	if err != nil {
		return Deployment{}, err
	}
	_ = s.audit(ctx, id, "extend_lease", map[string]any{
		"lease_seconds": req.LeaseSeconds,
		"reason":        req.Reason,
	})
	return dep, nil
}

func (s *Store) Archive(ctx context.Context, id string) (Deployment, error) {
	return s.setStatus(ctx, id, "archived")
}

func (s *Store) Restore(ctx context.Context, id string) (Deployment, error) {
	return s.setStatus(ctx, id, "active")
}

func (s *Store) Delete(ctx context.Context, id string) error {
	if err := s.Ensure(ctx); err != nil {
		return err
	}
	dep, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	backups, err := s.Backups(ctx, id)
	if err != nil {
		return err
	}
	decision := CleanupDecisionFor(dep, backups, time.Now().UTC())
	if !decision.CanDelete {
		if decision.Action == "wait_for_verified_backup" {
			return ErrRestoreRequired
		}
		return fmt.Errorf("%w: %s", ErrDeleteBlocked, decision.Reason)
	}
	_, err = s.pool.Exec(ctx, `/* pg_sage */
		WITH terminal AS (
			UPDATE sage.agent_db_deployments
			SET status='deleted', updated_at=now()
			WHERE deployment_id=$1
			RETURNING deployment_id
		)
		UPDATE sage.agent_db_monitoring_work AS work
		SET status='revoked', revoked_at=now(), claim_id='', claim_owner='',
			claim_expires_at=NULL, updated_at=now()
		FROM terminal
		WHERE work.deployment_id=terminal.deployment_id`,
		id,
	)
	if err != nil {
		return err
	}
	return s.audit(ctx, id, "delete", nil)
}

func (s *Store) CleanupDecision(
	ctx context.Context,
	id string,
	now time.Time,
) (CleanupDecision, error) {
	dep, err := s.Get(ctx, id)
	if err != nil {
		return CleanupDecision{}, err
	}
	backups, err := s.Backups(ctx, id)
	if err != nil {
		return CleanupDecision{}, err
	}
	return CleanupDecisionFor(dep, backups, now), nil
}

func (s *Store) ArchiveExpired(
	ctx context.Context,
	now time.Time,
) ([]Deployment, error) {
	if err := s.Ensure(ctx); err != nil {
		return nil, err
	}
	return s.claimExpiredDeployments(ctx, now)
}

func (s *Store) setStatus(ctx context.Context, id, status string) (Deployment, error) {
	return s.setStatusFields(ctx, id, status, "")
}

func (s *Store) setStatusFields(
	ctx context.Context,
	id string,
	status string,
	extra string,
) (Deployment, error) {
	update := "status=$2, updated_at=now()"
	if extra != "" {
		update += ", " + extra
	}
	update += ", lifecycle_version=lifecycle_version+1"
	if status == "active" {
		update += ", cleanup_claim_id='', cleanup_claimed_at=NULL, " +
			"teardown_operation_id='', provider_mutation_id='', " +
			"provider_mutation_expires_at=NULL"
	}
	tag, err := s.pool.Exec(ctx, `/* pg_sage */ 
		UPDATE sage.agent_db_deployments
		SET `+update+`
		WHERE deployment_id=$1`,
		id, status,
	)
	if err != nil {
		return Deployment{}, err
	}
	if tag.RowsAffected() == 0 {
		return Deployment{}, ErrNotFound
	}
	_ = s.audit(ctx, id, status, nil)
	return s.Get(ctx, id)
}

func sameRequest(old Request, req RequestCreate) (Request, error) {
	if old.BodyHash != bodyHash(req.Body) {
		return Request{}, ErrConflict
	}
	return old, nil
}

func requestBody(req RequestCreate) map[string]any {
	return map[string]any{
		"agent_id":                 req.AgentID,
		"allowed_regions":          req.AllowedRegions,
		"approval_sla_seconds":     req.ApprovalSLASeconds,
		"budget_usd":               req.BudgetUSD,
		"database_name":            req.DatabaseName,
		"data_classification":      req.DataClassification,
		"masking_policy_id":        req.MaskingPolicyID,
		"provider":                 req.Provider,
		"region":                   req.Region,
		"requested_isolation_type": req.IsolationType,
		"tenant_id":                req.TenantID,
	}
}

func (s *Store) insertRequest(
	ctx context.Context,
	req RequestCreate,
	hash string,
	dec PolicyDecision,
) (Request, error) {
	var out Request
	err := scanRequest(s.pool.QueryRow(ctx, insertRequestSQL,
		req.RequestID,
		req.TenantID,
		req.AgentID,
		req.OwnerID,
		req.RunID,
		req.Purpose,
		req.IsolationType,
		req.DatabaseName,
		req.Provider,
		dec.Decision,
		dec.Status,
		req.IdempotencyKey,
		hash,
		req.BudgetUSD,
		req.BackupRequired,
		jsonBytes(policyReasons(dec)),
		policyDecider(dec),
	), &out)
	return out, err
}

func (s *Store) requestByIdempotency(
	ctx context.Context,
	tenantID string,
	key string,
) (Request, error) {
	var req Request
	err := scanRequest(s.pool.QueryRow(ctx, selectRequestsSQL+`
		WHERE tenant_id=$1 AND idempotency_key=$2`,
		tenantID, key,
	), &req)
	if errors.Is(err, pgx.ErrNoRows) {
		return Request{}, ErrNotFound
	}
	return req, err
}

func scanRequest(row scanner, req *Request) error {
	return row.Scan(
		&req.RequestID,
		&req.TenantID,
		&req.AgentID,
		&req.OwnerID,
		&req.RunID,
		&req.Purpose,
		&req.IsolationType,
		&req.DatabaseName,
		&req.Provider,
		&req.PolicyDecision,
		&req.Status,
		&req.IdempotencyKey,
		&req.BodyHash,
		&req.BudgetUSD,
		&req.BackupRequired,
		&req.PolicyReasons,
		&req.DecidedBy,
		&req.DecidedAt,
		&req.ConsumedDeploymentID,
		&req.ConsumedBy,
		&req.ConsumedAt,
		&req.CreatedAt,
		&req.UpdatedAt,
	)
}

func scanDeployment(row scanner, dep *Deployment) error {
	return row.Scan(
		&dep.DeploymentID,
		&dep.TenantID,
		&dep.AgentID,
		&dep.RunID,
		&dep.DatabaseName,
		&dep.Status,
		&dep.SafetyMode,
		&dep.IsolationType,
		&dep.SchemaName,
		&dep.Provider,
		&dep.ProvisioningLevel,
		&dep.SizeProfileID,
		&dep.ProvisioningStatus,
		&dep.ProviderResourceID,
		&dep.SecretRef,
		&dep.SecretRefProvider,
		&dep.SecretRefExpiresAt,
		&dep.LiveMode,
		&dep.BudgetUSD,
		&dep.BackupRequired,
		&dep.CreatedAt,
		&dep.UpdatedAt,
		&dep.LastPingAt,
		&dep.LeaseExpiresAt,
		&dep.Metadata,
		&dep.ProvisioningPlan,
		&dep.ConnectionInfo,
		&dep.LifecycleVersion,
		&dep.CleanupClaimID,
		&dep.CleanupClaimedAt,
		&dep.TeardownOperationID,
		&dep.ProviderMutationID,
		&dep.ProviderMutationExpiresAt,
		&dep.AgentStatus,
		&dep.TeardownBlockedReason,
		&dep.TeardownBlockedAt,
		&dep.CreateOperationID,
	)
}
