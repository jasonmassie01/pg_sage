package fleet

import (
	"testing"
)

// On PostgreSQL: a failed pass must not hide a member's wait from an
// earlier pass, and a pass left running is closed as failed when the next
// one starts (review-04b review).

func TestStoreDB_FailedPassKeepsTheEarlierWait(t *testing.T) {
	pool, ctx := livePool(t, "failedwait")
	s := NewStore(pool, "meta")
	rs := rollupsOf(synth(3, 1, 2))
	nov := startTriaged(t, s, "2026-11", rs)
	for _, db := range []string{"db0001", "db0002"} {
		if err := s.MarkDeferred(ctx, nov, db, "2026-12"); err != nil {
			t.Fatalf("defer %s: %v", db, err)
		}
	}
	finish(t, s, nov, PassDone)
	// December's pass triages both again, schedules db0001, then fails.
	dec := startTriaged(t, s, "2026-12", rs)
	d := Deep{Database: "db0001", PassID: dec, Tokens: 10, Period: "2026-12"}
	if err := s.MarkScheduled(ctx, d, 7); err != nil {
		t.Fatalf("schedule db0001: %v", err)
	}
	finish(t, s, dec, PassFailed)
	carried, err := s.Carried(ctx, "2026-12")
	if err != nil || len(carried) != 1 || carried[0].Database != "db0002" ||
		carried[0].PassID != nov {
		t.Fatalf("carried after a failed pass = %+v (%v), want db0002 from pass %d",
			carried, err, nov)
	}
	// db0001 was scheduled in the failed pass: it waits no more, next
	// month either.
	if carried, err := s.Carried(ctx, "2027-01"); err != nil || len(carried) != 1 ||
		carried[0].Database != "db0002" {
		t.Fatalf("carried into 2027-01 = %+v (%v), want only db0002", carried, err)
	}
}

func TestStoreDB_StartPassClosesAPassLeftRunning(t *testing.T) {
	pool, ctx := livePool(t, "stalepass")
	s := NewStore(pool, "meta")
	other := NewStore(pool, "other")
	cut, err := s.StartPass(ctx, "2026-11", 3)
	if err != nil {
		t.Fatalf("start the cut-off pass: %v", err)
	}
	theirs, err := other.StartPass(ctx, "2026-11", 1)
	if err != nil {
		t.Fatalf("start another scope's pass: %v", err)
	}
	next, err := s.StartPass(ctx, "2026-11", 3)
	if err != nil || next <= cut {
		t.Fatalf("start the next pass: %d (%v), want after %d", next, err, cut)
	}
	want := map[int64]string{cut: PassFailed, theirs: PassRunning, next: PassRunning}
	for id, status := range want {
		var got, why string
		var finished bool
		if err := pool.QueryRow(ctx, `SELECT status, error, finished_at IS NOT NULL
			FROM sage.review_fleet_pass WHERE id = $1`, id).Scan(&got, &why,
			&finished); err != nil {
			t.Fatalf("read pass %d: %v", id, err)
		}
		if got != status || finished != (status != PassRunning) ||
			(status == PassFailed) != (why == abandonedPass) {
			t.Fatalf("pass %d is %s (finished %v, error %q), want %s", id, got, finished,
				why, status)
		}
	}
	if st, ok, err := s.LastPass(ctx, "2026-11"); err != nil || !ok || st != PassFailed {
		t.Fatalf("last pass = %q %v %v, want the closed one, failed", st, ok, err)
	}
}

// startTriaged starts a pass of period over rs with the deterministic
// triage's outliers triaged.
func startTriaged(t *testing.T, s *Store, period string, rs []Rollup) int64 {
	t.Helper()
	ctx := t.Context()
	pass, err := s.StartPass(ctx, period, len(rs))
	if err != nil {
		t.Fatalf("start %s: %v", period, err)
	}
	if err := s.SaveRollups(ctx, pass, rs); err != nil {
		t.Fatalf("save the rollups of %s: %v", period, err)
	}
	if err := s.MarkTriaged(ctx, pass, DeterministicTriage(rs)); err != nil {
		t.Fatalf("triage %s: %v", period, err)
	}
	return pass
}

func finish(t *testing.T, s *Store, pass int64, status string) {
	t.Helper()
	if err := s.FinishPass(t.Context(), pass, PassSummary{Status: status}); err != nil {
		t.Fatalf("finish pass %d as %s: %v", pass, status, err)
	}
}
