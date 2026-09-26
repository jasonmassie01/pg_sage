package agentdb

import (
	"context"
	"testing"
	"time"
)

// SURF-08: with more physical targets than one pass can schedule, later
// passes must reach the unscheduled targets instead of re-selecting the
// same first 100 by deployment id; due targets are not re-enqueued early.
func TestMonitoringScheduleRotatesAcrossPasses(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	const tenant = "fix_rotation_tenant"
	cleanup := func() {
		for _, sql := range []string{
			`DELETE FROM sage.agent_db_monitoring_work WHERE tenant_id=$1`,
			`DELETE FROM sage.agent_db_monitoring_state WHERE tenant_id=$1`,
			`DELETE FROM sage.agent_db_deployments WHERE tenant_id=$1`,
		} {
			_, _ = pool.Exec(context.Background(), sql, tenant)
		}
	}
	cleanup()
	defer cleanup()
	if _, err := pool.Exec(ctx, `INSERT INTO sage.agent_db_deployments
		(deployment_id, tenant_id, agent_id, database_name, status, provider,
		 provisioning_level, provisioning_status, provider_resource_id, lease_expires_at)
		SELECT 'fix_rot_'||g, $1, 'agent', 'db', 'active', 'aws_rds', 'schema',
		 'available', 'fix-phys-'||g, now()+interval '1 hour'
		FROM generate_series(1, 150) AS g`, tenant); err != nil {
		t.Fatalf("seed: %v", err)
	}
	now := time.Now().UTC()
	for pass := 0; pass < 2; pass++ {
		if _, err := st.ScheduleMonitoring(ctx, now, 10_000); err != nil {
			t.Fatalf("ScheduleMonitoring pass %d: %v", pass, err)
		}
	}
	var scheduled int
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT physical_target_key)
		FROM sage.agent_db_monitoring_state WHERE tenant_id=$1`, tenant).Scan(&scheduled); err != nil {
		t.Fatal(err)
	}
	if scheduled != 150 {
		t.Fatalf("two passes scheduled %d of 150 targets (starvation)", scheduled)
	}
}
