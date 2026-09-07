package agentdb

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func upsertWave3MonitoringPolicy(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	scopeType string,
	scopeID string,
	maximum int,
) {
	t.Helper()
	var previous int
	err := pool.QueryRow(ctx, `SELECT max_concurrency
		FROM sage.agent_db_monitoring_policies
		WHERE scope_type=$1 AND scope_id=$2`, scopeType, scopeID).Scan(&previous)
	existed := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("read monitoring policy: %v", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO sage.agent_db_monitoring_policies
		(scope_type, scope_id, max_concurrency) VALUES ($1,$2,$3)
		ON CONFLICT (scope_type, scope_id) DO UPDATE
		SET max_concurrency=EXCLUDED.max_concurrency, updated_at=now()`,
		scopeType, scopeID, maximum)
	if err != nil {
		t.Fatalf("upsert monitoring policy: %v", err)
	}
	t.Cleanup(func() {
		if existed {
			_, _ = pool.Exec(ctx, `UPDATE sage.agent_db_monitoring_policies
				SET max_concurrency=$3, updated_at=now()
				WHERE scope_type=$1 AND scope_id=$2`, scopeType, scopeID, previous)
			return
		}
		_, _ = pool.Exec(ctx, `DELETE FROM sage.agent_db_monitoring_policies
			WHERE scope_type=$1 AND scope_id=$2`, scopeType, scopeID)
	})
}

func assertWave3WorkTableHasNoSecretColumn(t *testing.T, ctx context.Context,
	pool *pgxpool.Pool) {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_schema='sage' AND table_name='agent_db_monitoring_work'`)
	if err != nil {
		t.Fatalf("list monitoring work columns: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatalf("scan monitoring work column: %v", err)
		}
		if column == "password" || column == "secret_value" ||
			column == "resolved_secret" || column == "connection_string" {
			t.Fatalf("monitoring work persists secret-bearing column %q", column)
		}
	}
}

func seedWave3TenThousandDeployments(t *testing.T, ctx context.Context,
	pool *pgxpool.Pool, tenant string) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO sage.agent_db_deployments
		(deployment_id, tenant_id, agent_id, database_name, status, safety_mode,
		 isolation_type, schema_name, provider, provisioning_level,
		 provisioning_status, provider_resource_id, backup_required,
		 lease_expires_at, metadata)
		SELECT 'wave3_10k_'||g, $1, 'agent-'||g, 'app_'||(g % 100),
		 'active', 'observation', 'schema', 'schema_'||g, 'aws_rds', 'schema',
		 'available', 'physical-10k-'||(g % 100), false, now()+interval '1 hour',
		 '{"monitoring_trigger":"baseline","serverless_state":"active"}'::jsonb
		FROM generate_series(1, 10000) AS g`, tenant)
	if err != nil {
		t.Fatalf("seed 10k deployments: %v", err)
	}
}

func assertWave3PhysicalWorkGrouping(t *testing.T, ctx context.Context,
	pool *pgxpool.Pool, tenant string) {
	t.Helper()
	var workRows, targets int
	err := pool.QueryRow(ctx, `SELECT count(*), count(DISTINCT physical_target_key)
		FROM sage.agent_db_monitoring_work
		WHERE tenant_id=$1 AND tier IN ('light_sql_probe','deep_analysis')`,
		tenant).Scan(&workRows, &targets)
	if err != nil {
		t.Fatalf("count grouped monitoring work: %v", err)
	}
	if workRows > 100 || targets != 100 {
		t.Fatalf("10k schema rows produced %d SQL jobs for %d targets",
			workRows, targets)
	}
}
