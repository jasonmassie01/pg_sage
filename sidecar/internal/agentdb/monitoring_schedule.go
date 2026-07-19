package agentdb

import (
	"context"
	"time"
)

const maxMonitoringScanRows = 10_000
const maxMonitoringTargetsPerPass = 100

type MonitoringScheduleResult struct {
	ScannedRows     int `json:"scanned_rows"`
	Enqueued        int `json:"enqueued"`
	PhysicalTargets int `json:"physical_targets"`
}

func (s *Store) ScheduleMonitoring(
	ctx context.Context,
	now time.Time,
	scanLimit int,
) (MonitoringScheduleResult, error) {
	if scanLimit <= 0 {
		return MonitoringScheduleResult{}, ErrInvalid
	}
	if scanLimit > maxMonitoringScanRows {
		scanLimit = maxMonitoringScanRows
	}
	if err := s.Ensure(ctx); err != nil {
		return MonitoringScheduleResult{}, err
	}
	var result MonitoringScheduleResult
	err := s.pool.QueryRow(
		ctx, scheduleMonitoringSQL, now, scanLimit, maxMonitoringTargetsPerPass,
	).Scan(
		&result.ScannedRows,
		&result.PhysicalTargets,
		&result.Enqueued,
	)
	return result, err
}

const scheduleMonitoringSQL = `/* pg_sage */
	WITH selected AS MATERIALIZED (
		SELECT deployment_id, tenant_id, provider, provider_resource_id,
			database_name
		FROM sage.agent_db_deployments
		WHERE status = 'active'
			AND monitoring_mode = 'adaptive'
		ORDER BY deployment_id
		LIMIT $2
	), physical AS MATERIALIZED (
		SELECT deployment_id, tenant_id, provider,
			tenant_id || '|' || provider || '|' ||
			COALESCE(NULLIF(provider_resource_id, ''),
				NULLIF(database_name, ''), deployment_id) || '|' ||
			database_name AS physical_target_key
		FROM selected
	), targets AS MATERIALIZED (
		SELECT min(deployment_id) AS deployment_id, tenant_id, provider,
			physical_target_key
		FROM physical
		GROUP BY tenant_id, provider, physical_target_key
		ORDER BY count(*) DESC, physical_target_key
		LIMIT $3
	), state_upsert AS (
		INSERT INTO sage.agent_db_monitoring_state AS current (
			physical_target_key, deployment_id, tenant_id, provider,
			next_due_at, last_scheduled_at
		)
		SELECT physical_target_key, deployment_id, tenant_id, provider,
			$1::timestamptz + interval '5 minutes', $1::timestamptz
		FROM targets
		ON CONFLICT (physical_target_key) DO UPDATE
		SET deployment_id = EXCLUDED.deployment_id,
			tenant_id = EXCLUDED.tenant_id,
			provider = EXCLUDED.provider,
			next_due_at = EXCLUDED.next_due_at,
			last_scheduled_at = EXCLUDED.last_scheduled_at,
			updated_at = now()
		RETURNING physical_target_key
	), work_upsert AS (
		INSERT INTO sage.agent_db_monitoring_work AS current (
			work_id, deployment_id, tenant_id, provider,
			physical_target_key, tier, status, next_due_at
		)
		SELECT 'monitor-' || md5(physical_target_key || ':light_sql_probe'),
			deployment_id, tenant_id, provider, physical_target_key,
			'light_sql_probe', 'queued', $1::timestamptz
		FROM targets
		ON CONFLICT (work_id) DO UPDATE
		SET deployment_id = EXCLUDED.deployment_id,
			tenant_id = EXCLUDED.tenant_id,
			provider = EXCLUDED.provider,
			physical_target_key = EXCLUDED.physical_target_key,
			next_due_at = CASE
				WHEN current.status IN ('completed', 'failed', 'revoked')
					THEN EXCLUDED.next_due_at
				ELSE current.next_due_at
			END,
			status = CASE
				WHEN current.status IN ('completed', 'failed', 'revoked')
					THEN 'queued'
				ELSE current.status
			END,
			claim_id = CASE
				WHEN current.status IN ('completed', 'failed', 'revoked')
					THEN ''
				ELSE current.claim_id
			END,
			claim_owner = CASE
				WHEN current.status IN ('completed', 'failed', 'revoked')
					THEN ''
				ELSE current.claim_owner
			END,
			claim_expires_at = CASE
				WHEN current.status IN ('completed', 'failed', 'revoked')
					THEN NULL
				ELSE current.claim_expires_at
			END,
			revoked_at = NULL,
			updated_at = now()
		RETURNING work_id
	)
	SELECT (SELECT count(*) FROM selected),
		(SELECT count(*) FROM targets),
		(SELECT count(*) FROM work_upsert)`
