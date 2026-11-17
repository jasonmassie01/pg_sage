package collector

import (
	"context"
	"testing"
)

// review-02e on a live server: workload_sessions counts this database's
// client sessions by application and connection minute, and leaves
// pg_sage's own out.
func TestWorkloadSessions_CountsAppAgesByMinuteWithoutSelf(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		conn := openSession(t, pool, "wl_appage_sync")
		if _, err := conn.Exec(ctx, "SELECT 1"); err != nil {
			t.Fatal(err)
		}
	}
	self := openSession(t, pool, "pg_sage")
	if _, err := self.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	snap := &Snapshot{}
	if err := workloadStep(t, c).run(ctx, snap); err != nil {
		t.Fatalf("workload_sessions step: %v", err)
	}
	ws := snap.WorkloadSessions
	if ws == nil || ws.AppAges == nil || ws.AppAgesTruncated {
		t.Fatalf("app ages %+v, want a whole list after a successful read", ws)
	}
	synced := 0
	for _, a := range ws.AppAges {
		switch a.Application {
		case "pg_sage":
			t.Errorf("pg_sage's own session counted: %+v", a)
		case "wl_appage_sync":
			if a.Minute == nil || *a.Minute != 0 {
				t.Errorf("a session opened just now is at minute %v", a.Minute)
			}
			synced += a.Sessions
		}
	}
	if synced != 3 {
		t.Fatalf("wl_appage_sync counted %d sessions in %+v, want 3", synced, ws.AppAges)
	}
}
