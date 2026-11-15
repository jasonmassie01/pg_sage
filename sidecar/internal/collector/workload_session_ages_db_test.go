package collector

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// evid-02b on a live server: workload_sessions counts this database's
// client sessions by state and connection-age bucket (now() -
// backend_start), leaves pg_sage's own out, and buckets the 1 min, 10 min
// and 60 min boundaries into the bucket that starts there.

func TestWorkloadAgeBucketSQL_Boundaries(t *testing.T) {
	pool := testPool(t)
	for age, want := range map[string]string{
		"0 seconds": AgeUnder1m, "59.999 seconds": AgeUnder1m, "1 minute": Age1To10m,
		"9 minutes 59 seconds": Age1To10m, "10 minutes": Age10To60m,
		"59 minutes 59 seconds": Age10To60m, "60 minutes": AgeOver1h, "3 days": AgeOver1h,
	} {
		var got string
		err := pool.QueryRow(context.Background(),
			"SELECT "+ageBucketSQL("$1::interval"), age).Scan(&got)
		if err != nil || got != want {
			t.Errorf("age %s: bucket %q (%v), want %q", age, got, err, want)
		}
	}
}

func TestWorkloadSessions_CountsAgesByStateWithoutSelf(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	idle := openSession(t, pool, "wl_age_idle")
	if _, err := idle.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
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
	if ws == nil || ws.Ages == nil {
		t.Fatalf("ages %+v, want a non-nil list after a successful read", ws)
	}
	groupIdle := 0
	for _, g := range ws.Groups {
		if g.State == "idle" {
			groupIdle += g.Sessions
		}
	}
	agesIdle := 0
	for _, a := range ws.Ages {
		if a.Sessions <= 0 || !validAgeBucket(a.Bucket) {
			t.Errorf("age row %+v", a)
		}
		if a.State == "idle" {
			agesIdle += a.Sessions
			if a.Bucket != AgeUnder1m {
				t.Errorf("an idle session opened just now is in %s", a.Bucket)
			}
		}
	}
	// The groups leave pg_sage out the same way, so both count the same
	// idle sessions: ours (wl_age_idle) and nobody else's pg_sage.
	if agesIdle == 0 || agesIdle != groupIdle {
		t.Fatalf("ages count %d idle sessions, groups %d", agesIdle, groupIdle)
	}
	raw, err := json.Marshal(ws)
	if err != nil || !strings.Contains(string(raw), `"ages":[`) {
		t.Fatalf("encoded %s (%v), want an ages list", raw, err)
	}
}

func validAgeBucket(b string) bool {
	switch b {
	case AgeUnder1m, Age1To10m, Age10To60m, AgeOver1h:
		return true
	}
	return false
}
