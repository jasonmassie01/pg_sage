package agentdb

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const maxMonitoringClaimBatch = 100

type MonitoringClaim struct {
	WorkID            string    `json:"work_id"`
	DeploymentID      string    `json:"deployment_id"`
	PhysicalTargetKey string    `json:"physical_target_key"`
	TenantID          string    `json:"tenant_id"`
	Provider          string    `json:"provider"`
	Tier              string    `json:"tier"`
	ClaimID           string    `json:"claim_id"`
	ClaimOwner        string    `json:"claim_owner"`
	ClaimExpiresAt    time.Time `json:"claim_expires_at"`
	SecretRef         string    `json:"secret_ref,omitempty"`
}

type monitoringCandidate struct {
	workID            string
	deploymentID      string
	tenantID          string
	provider          string
	physicalTargetKey string
	tier              string
	secretRef         string
}

type monitoringCounts struct {
	global      int
	providers   map[string]int
	tenants     map[string]int
	deployments map[string]int
}

type monitoringPolicies map[string]int

func (s *Store) ClaimMonitoringWork(
	ctx context.Context,
	owner string,
	now time.Time,
	lease time.Duration,
	limit int,
) ([]MonitoringClaim, error) {
	limit, err := normalizeMonitoringClaimInput(owner, lease, limit)
	if err != nil {
		return nil, err
	}
	if err := s.Ensure(ctx); err != nil {
		return nil, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, monitoringClaimAdvisoryLockSQL); err != nil {
		return nil, err
	}
	policies, err := readMonitoringPolicies(ctx, tx)
	if err != nil {
		return nil, err
	}
	counts, err := readMonitoringCounts(ctx, tx, now)
	if err != nil {
		return nil, err
	}
	claims, err := claimEligibleMonitoringWork(
		ctx, tx, owner, now, lease, limit, policies, counts,
	)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return claims, nil
}

func normalizeMonitoringClaimInput(
	owner string,
	lease time.Duration,
	limit int,
) (int, error) {
	if owner == "" || lease <= 0 || limit <= 0 {
		return 0, ErrInvalid
	}
	if limit > maxMonitoringClaimBatch {
		limit = maxMonitoringClaimBatch
	}
	return limit, nil
}

func readMonitoringPolicies(
	ctx context.Context,
	tx pgx.Tx,
) (monitoringPolicies, error) {
	rows, err := tx.Query(ctx, `/* pg_sage */
		SELECT scope_type, scope_id, max_concurrency
		FROM sage.agent_db_monitoring_policies
		WHERE max_concurrency > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	policies := monitoringPolicies{}
	for rows.Next() {
		var scopeType, scopeID string
		var maxConcurrency int
		if err := rows.Scan(&scopeType, &scopeID, &maxConcurrency); err != nil {
			return nil, err
		}
		policies[monitoringPolicyKey(scopeType, scopeID)] = maxConcurrency
	}
	return policies, rows.Err()
}

func readMonitoringCounts(
	ctx context.Context,
	tx pgx.Tx,
	now time.Time,
) (monitoringCounts, error) {
	counts := newMonitoringCounts()
	rows, err := tx.Query(ctx, `/* pg_sage */
		SELECT provider, tenant_id, deployment_id, count(*)
		FROM sage.agent_db_monitoring_work
		WHERE status = 'claimed' AND claim_expires_at > $1
			AND revoked_at IS NULL
		GROUP BY provider, tenant_id, deployment_id`, now)
	if err != nil {
		return counts, err
	}
	defer rows.Close()
	for rows.Next() {
		var provider, tenantID, deploymentID string
		var count int
		if err := rows.Scan(&provider, &tenantID, &deploymentID, &count); err != nil {
			return counts, err
		}
		counts.global += count
		counts.providers[provider] += count
		counts.tenants[tenantID] += count
		counts.deployments[deploymentID] += count
	}
	return counts, rows.Err()
}

func newMonitoringCounts() monitoringCounts {
	return monitoringCounts{
		providers:   map[string]int{},
		tenants:     map[string]int{},
		deployments: map[string]int{},
	}
}

func claimEligibleMonitoringWork(
	ctx context.Context,
	tx pgx.Tx,
	owner string,
	now time.Time,
	lease time.Duration,
	limit int,
	policies monitoringPolicies,
	counts monitoringCounts,
) ([]MonitoringClaim, error) {
	remaining := monitoringGlobalLimit(limit, policies) - counts.global
	if remaining <= 0 {
		return []MonitoringClaim{}, nil
	}
	candidates, err := lockMonitoringCandidates(ctx, tx, now, limit*16)
	if err != nil {
		return nil, err
	}
	claims := make([]MonitoringClaim, 0, remaining)
	for _, candidate := range candidates {
		if len(claims) >= remaining {
			break
		}
		if !monitoringPolicyAllows(candidate, policies, counts) {
			continue
		}
		claim, err := persistMonitoringClaim(
			ctx, tx, candidate, owner, now, lease, len(claims),
		)
		if err != nil {
			return nil, err
		}
		claims = append(claims, claim)
		incrementMonitoringCounts(&counts, candidate)
	}
	return claims, nil
}

func monitoringGlobalLimit(limit int, policies monitoringPolicies) int {
	for key, maximum := range policies {
		if strings.HasPrefix(key, "global\x00") && maximum > 0 && maximum < limit {
			limit = maximum
		}
	}
	return limit
}

func lockMonitoringCandidates(
	ctx context.Context,
	tx pgx.Tx,
	now time.Time,
	limit int,
) ([]monitoringCandidate, error) {
	rows, err := tx.Query(ctx, monitoringCandidatesSQL, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	candidates := make([]monitoringCandidate, 0, limit)
	for rows.Next() {
		var candidate monitoringCandidate
		err := rows.Scan(
			&candidate.workID,
			&candidate.deploymentID,
			&candidate.tenantID,
			&candidate.provider,
			&candidate.physicalTargetKey,
			&candidate.tier,
			&candidate.secretRef,
		)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

func monitoringPolicyAllows(
	candidate monitoringCandidate,
	policies monitoringPolicies,
	counts monitoringCounts,
) bool {
	checks := []struct {
		scopeType string
		scopeID   string
		count     int
	}{
		{"provider", candidate.provider, counts.providers[candidate.provider]},
		{"tenant", candidate.tenantID, counts.tenants[candidate.tenantID]},
		{"deployment", candidate.deploymentID,
			counts.deployments[candidate.deploymentID]},
	}
	for _, check := range checks {
		maximum := policies[monitoringPolicyKey(check.scopeType, check.scopeID)]
		if maximum > 0 && check.count >= maximum {
			return false
		}
	}
	return true
}

func persistMonitoringClaim(
	ctx context.Context,
	tx pgx.Tx,
	candidate monitoringCandidate,
	owner string,
	now time.Time,
	lease time.Duration,
	sequence int,
) (MonitoringClaim, error) {
	expiresAt := now.Add(lease)
	claimID := idFrom(
		"monitoring-claim", owner, candidate.workID,
		expiresAt.Format(time.RFC3339Nano), fmt.Sprint(sequence),
	)
	tag, err := tx.Exec(ctx, `/* pg_sage */
		UPDATE sage.agent_db_monitoring_work
		SET status='claimed', claim_id=$2, claim_owner=$3,
			claim_expires_at=$4, updated_at=now()
		WHERE work_id=$1`, candidate.workID, claimID, owner, expiresAt)
	if err != nil {
		return MonitoringClaim{}, err
	}
	if tag.RowsAffected() != 1 {
		return MonitoringClaim{}, ErrConflict
	}
	return monitoringClaim(candidate, claimID, owner, expiresAt), nil
}

func monitoringClaim(
	candidate monitoringCandidate,
	claimID string,
	owner string,
	expiresAt time.Time,
) MonitoringClaim {
	return MonitoringClaim{
		WorkID:            candidate.workID,
		DeploymentID:      candidate.deploymentID,
		PhysicalTargetKey: candidate.physicalTargetKey,
		TenantID:          candidate.tenantID,
		Provider:          candidate.provider,
		Tier:              candidate.tier,
		ClaimID:           claimID,
		ClaimOwner:        owner,
		ClaimExpiresAt:    expiresAt,
		SecretRef:         candidate.secretRef,
	}
}

func incrementMonitoringCounts(
	counts *monitoringCounts,
	candidate monitoringCandidate,
) {
	counts.global++
	counts.providers[candidate.provider]++
	counts.tenants[candidate.tenantID]++
	counts.deployments[candidate.deploymentID]++
}

func monitoringPolicyKey(scopeType, scopeID string) string {
	return scopeType + "\x00" + scopeID
}

const monitoringClaimAdvisoryLockSQL = `/* pg_sage */
	SELECT pg_advisory_xact_lock(hashtext('pg_sage_agentdb_monitoring_claims'))`

const monitoringCandidatesSQL = `/* pg_sage */
	SELECT work.work_id, work.deployment_id, work.tenant_id, work.provider,
		work.physical_target_key, work.tier, deployment.secret_ref
	FROM sage.agent_db_monitoring_work AS work
	JOIN sage.agent_db_deployments AS deployment
		ON deployment.deployment_id = work.deployment_id
	WHERE work.revoked_at IS NULL
		AND work.next_due_at <= $1
		AND (
			work.status = 'queued'
			OR (work.status = 'claimed' AND work.claim_expires_at <= $1)
		)
		AND deployment.status = 'active'
		AND deployment.monitoring_mode <> 'disabled'
		AND (
			work.tier = 'lifecycle_provider'
			OR COALESCE(deployment.metadata->>'serverless_state', 'active') <> 'idle'
			OR deployment.wake_idle_allowed
		)
	ORDER BY work.next_due_at, work.work_id
	FOR UPDATE OF work SKIP LOCKED
	LIMIT $2`
