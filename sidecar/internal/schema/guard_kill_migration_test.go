package schema

import (
	"strconv"
	"testing"
)

// Kill switch and freeze state (AGENTDB-SPEC §6.10, §7): guard_kills (the
// durable kill report), guard_freezes (principal, database and fleet
// flags, at most one open per target), guard_unfreeze_requests (the
// two-person unfreeze) and guard_inflight (backends the kill cancels).

func TestGuardKillMigration_TablesAndChecks(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool) // idempotent
	for _, tbl := range []string{"guard_kills", "guard_freezes", "guard_unfreeze_requests",
		"guard_inflight"} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('sage.' || $1) IS NOT NULL`,
			tbl).Scan(&ok); err != nil || !ok {
			t.Fatalf("table %s: %v %v", tbl, ok, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.guard_unfreeze_requests
			WHERE requested_by = 'mig-test'`)
		_, _ = pool.Exec(ctx, `DELETE FROM sage.guard_freezes WHERE set_by = 'mig-test'`)
		_, _ = pool.Exec(ctx, `DELETE FROM sage.guard_kills WHERE requested_by = 'mig-test'`)
		_, _ = pool.Exec(ctx, `DELETE FROM sage.guard_inflight
			WHERE principal_id = 'agp_migtestmigtestmigtest'`)
	})
	var killID, freezeID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.guard_kills (scope, target, reason,
		requested_by) VALUES ('principal', 'x', 'r', 'mig-test') RETURNING id`).
		Scan(&killID); err != nil {
		t.Fatalf("valid kill: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO sage.guard_freezes (scope, target, kill_id,
		reason, set_by) VALUES ('database', 'mig-db', $1, 'r', 'mig-test') RETURNING id`,
		killID).Scan(&freezeID); err != nil {
		t.Fatalf("valid freeze: %v", err)
	}
	for name, sql := range map[string]string{
		"kill scope": `INSERT INTO sage.guard_kills (scope, reason, requested_by)
			VALUES ('fleet', 'r', 'mig-test')`,
		"kill empty reason": `INSERT INTO sage.guard_kills (scope, reason, requested_by)
			VALUES ('all', '', 'mig-test')`,
		"freeze scope": `INSERT INTO sage.guard_freezes (scope, target, reason, set_by)
			VALUES ('all', '', 'r', 'mig-test')`,
		"second open freeze": `INSERT INTO sage.guard_freezes (scope, target, reason, set_by)
			VALUES ('database', 'mig-db', 'r', 'mig-test')`,
		"request status": `INSERT INTO sage.guard_unfreeze_requests (scope, target, freeze_id,
			requested_by, requested_by_user, reason, expires_at, status)
			VALUES ('database', 'mig-db', ` + strconv.FormatInt(freezeID, 10) + `, 'mig-test', 1, 'r',
			now(), 'maybe')`,
		"inflight uuid": `INSERT INTO sage.guard_inflight (principal_id, database_id,
			backend_pid) VALUES ('agp_migtestmigtestmigtest', 'nope', 1)`,
	} {
		if _, err := pool.Exec(ctx, sql); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// A cleared freeze frees its slot for the next one.
	if _, err := pool.Exec(ctx, `UPDATE sage.guard_freezes SET cleared_at = now(),
		cleared_by = 'mig-test' WHERE id = $1`, freezeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.guard_freezes (scope, target, reason,
		set_by) VALUES ('database', 'mig-db', 'r', 'mig-test')`); err != nil {
		t.Fatalf("a new open freeze after the old one cleared: %v", err)
	}
	// One pending request per freeze.
	pending := `INSERT INTO sage.guard_unfreeze_requests (scope, target, freeze_id,
		requested_by, requested_by_user, reason, expires_at)
		VALUES ('database', 'mig-db', $1, 'mig-test', 1, 'r', now() + interval '1 hour')`
	if _, err := pool.Exec(ctx, pending, freezeID); err != nil {
		t.Fatalf("pending request: %v", err)
	}
	if _, err := pool.Exec(ctx, pending, freezeID); err == nil {
		t.Error("a second pending request for one freeze was accepted")
	}
}

// The kill's lookups use indexes (perf gate A): open approvals by
// principal, open freezes by target, in-flight backends by principal.
func TestGuardKillMigration_Indexes(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	for _, idx := range []string{"guard_freezes_open", "guard_unfreeze_requests_pending",
		"guard_inflight_principal", "action_queue_principal_open"} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('sage.' || $1) IS NOT NULL`,
			idx).Scan(&ok); err != nil || !ok {
			t.Fatalf("index %s: %v %v", idx, ok, err)
		}
	}
}
