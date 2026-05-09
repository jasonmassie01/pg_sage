package agentdb

import (
	"context"
	"errors"
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

func (s *Store) SetRequestDecision(
	ctx context.Context,
	id string,
	req DecisionRequest,
) (Request, error) {
	if err := s.Ensure(ctx); err != nil {
		return Request{}, err
	}
	policy, status, err := requestDecision(req.Decision)
	if err != nil {
		return Request{}, err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE sage.agent_db_requests
		SET status=$2,
			policy_decision=$3,
			policy_reasons=$4::jsonb,
			updated_at=now()
		WHERE request_id=$1`,
		id, status, policy, jsonBytes(map[string]any{"reason": req.Reason}),
	)
	if err != nil {
		return Request{}, err
	}
	if tag.RowsAffected() == 0 {
		return Request{}, ErrNotFound
	}
	return s.GetRequest(ctx, id)
}

func (s *Store) Register(ctx context.Context, req RegisterRequest) (Deployment, error) {
	if err := s.Ensure(ctx); err != nil {
		return Deployment{}, err
	}
	if err := normalizeRegister(&req); err != nil {
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
		req.BudgetUSD,
		req.BackupRequired,
		req.LeaseSeconds,
		jsonBytes(req.Metadata),
		jsonBytes(req.ProvisioningPlan),
		jsonBytes(req.ConnectionInfo),
	), &dep)
	if err != nil {
		return Deployment{}, err
	}
	_ = s.audit(ctx, req.DeploymentID, "register", nil)
	_ = s.seedTuningHints(ctx, req.DeploymentID, req.Metadata)
	return dep, nil
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
	if req.Status == "" {
		req.Status = "active"
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO sage.agent_db_pings(deployment_id, status, metrics)
		VALUES ($1, $2, $3::jsonb)`,
		id, req.Status, jsonBytes(req.Metrics),
	); err != nil {
		return Deployment{}, err
	}
	return s.setStatusFields(ctx, id, req.Status, "last_ping_at=now()")
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
	tag, err := s.pool.Exec(ctx, `
		UPDATE sage.agent_db_deployments
		SET lease_expires_at=now()+make_interval(secs => $2),
			updated_at=now()
		WHERE deployment_id=$1`,
		id, req.LeaseSeconds,
	)
	if err != nil {
		return Deployment{}, err
	}
	if tag.RowsAffected() == 0 {
		return Deployment{}, ErrNotFound
	}
	_ = s.audit(ctx, id, "extend_lease", map[string]any{
		"lease_seconds": req.LeaseSeconds,
		"reason":        req.Reason,
	})
	return s.Get(ctx, id)
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
		return ErrInvalid
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE sage.agent_db_deployments
		SET status='deleted', updated_at=now()
		WHERE deployment_id=$1`,
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
	rows, err := s.pool.Query(ctx, selectDeploymentsSQL+`
		WHERE status IN ('active', 'budget_exceeded')
			AND lease_expires_at IS NOT NULL
			AND lease_expires_at < $1
		FOR UPDATE`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	expired := []Deployment{}
	for rows.Next() {
		var dep Deployment
		if err := scanDeployment(rows, &dep); err != nil {
			return nil, err
		}
		expired = append(expired, dep)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	archived := make([]Deployment, 0, len(expired))
	for _, dep := range expired {
		next, err := s.Archive(ctx, dep.DeploymentID)
		if err != nil {
			return nil, err
		}
		archived = append(archived, next)
	}
	return archived, nil
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
	tag, err := s.pool.Exec(ctx, `
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

func requestDecision(decision string) (string, string, error) {
	switch decision {
	case "approved":
		return "allow", "approved", nil
	case "denied":
		return "deny", "denied", nil
	default:
		return "", "", ErrInvalid
	}
}

func normalizeRegister(req *RegisterRequest) error {
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
	req.BackupRequired = true
	return nil
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
		dec.Decision,
		dec.Status,
		req.IdempotencyKey,
		hash,
		req.BudgetUSD,
		true,
		jsonBytes(policyReasons(dec)),
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
		&req.PolicyDecision,
		&req.Status,
		&req.IdempotencyKey,
		&req.BodyHash,
		&req.BudgetUSD,
		&req.BackupRequired,
		&req.PolicyReasons,
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
		&dep.BudgetUSD,
		&dep.BackupRequired,
		&dep.CreatedAt,
		&dep.UpdatedAt,
		&dep.LastPingAt,
		&dep.LeaseExpiresAt,
		&dep.Metadata,
		&dep.ProvisioningPlan,
		&dep.ConnectionInfo,
	)
}
