package schema

import (
	"context"
	"testing"
)

// sage.ha_identity (Sage SRE follow-ups B) keeps each HA monitor's last
// observed node identity, so a failover while pg_sage was down is seen at
// startup. The migration is idempotent and the table refuses an unknown
// role or a non-positive timeline.

func TestHAIdentityMigration_IdempotentAndConstrained(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	if _, err := pool.Exec(ctx, ddlHAIdentity); err != nil {
		t.Fatalf("re-running the migration: %v", err)
	}
	key := "schema-test:" + t.Name()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.ha_identity WHERE monitor_key = $1", key)
	})
	if _, err := pool.Exec(ctx, `INSERT INTO sage.ha_identity
		(monitor_key, role, timeline_id, system_identifier, server_started_at,
		 last_change_at, observed_at)
		VALUES ($1, 'primary', 3, '7001', now(), NULL, now())`, key); err != nil {
		t.Fatalf("insert: %v", err)
	}
	for name, sql := range map[string]string{
		"unknown role": `UPDATE sage.ha_identity SET role = 'unknown' WHERE monitor_key = $1`,
		"zero timeline": `UPDATE sage.ha_identity SET timeline_id = 0
			WHERE monitor_key = $1`,
		"no observation time": `UPDATE sage.ha_identity SET observed_at = NULL
			WHERE monitor_key = $1`,
	} {
		if _, err := pool.Exec(ctx, sql, key); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.ha_identity (monitor_key, role,
		observed_at) VALUES ($1, 'replica', now())`, key); err == nil {
		t.Fatal("a second row for the same key was accepted")
	}
}
