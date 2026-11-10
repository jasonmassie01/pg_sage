package schema

import (
	"context"
	"testing"
)

// fleet-03 migrations: the lease's control identity and the follow-up
// claim columns. No concurrent category: Bootstrap holds its advisory
// lock, so one migration runs per database at a time.

func TestFleetControlMigrations_AddColumnsOnAFreshServerTwice(t *testing.T) {
	pool := freshSchemaPool(t, "fleet_control") // bootstraps twice
	want := map[string]string{
		"fleet_leader_lease.control_system_identifier":  "bigint",
		"fleet_leader_lease.control_datname":            "text",
		"approval_card_deliveries.followup_claim_epoch": "bigint",
		"approval_card_deliveries.followup_claimed_at":  "timestamp with time zone",
		"fleet_leader_lease.scope":                      "text",
		"approval_card_deliveries.followed_up_at":       "timestamp with time zone",
	}
	rows, err := pool.Query(context.Background(), `SELECT table_name || '.' || column_name,
		data_type FROM information_schema.columns WHERE table_schema = 'sage'
		AND table_name IN ('fleet_leader_lease', 'approval_card_deliveries')`)
	if err != nil {
		t.Fatalf("read columns: %v", err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var col, typ string
		if err := rows.Scan(&col, &typ); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[col] = typ
	}
	for col, typ := range want {
		if got[col] != typ {
			t.Errorf("column %s = %q, want %q", col, got[col], typ)
		}
	}
}

// A re-run is catalog-first: it completes while another session holds a
// lock that ALTER TABLE would wait on past lock_timeout.
func TestFleetControlMigrations_RerunTakesNoTableLock(t *testing.T) {
	ctx := context.Background()
	pool := freshSchemaPool(t, "fleet_control_lock")
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `LOCK TABLE sage.fleet_leader_lease,
		sage.approval_card_deliveries IN ACCESS SHARE MODE`); err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	for name, ddl := range map[string]string{
		"ddlFleetLeaseControl":     ddlFleetLeaseControl,
		"ddlApprovalFollowupClaim": ddlApprovalFollowupClaim,
	} {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin %s: %v", name, err)
		}
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '200ms'`); err != nil {
			t.Fatalf("set timeout: %v", err)
		}
		_, err = tx.Exec(ctx, ddl)
		_ = tx.Rollback(ctx)
		if err != nil {
			t.Fatalf("re-run %s under a held lock: %v", name, err)
		}
	}
}
