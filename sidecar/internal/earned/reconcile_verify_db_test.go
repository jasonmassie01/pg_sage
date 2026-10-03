package earned

import (
	"testing"
	"time"
)

// P0-6: only an outcome with a completed verification verdict is a
// verified recovery. An action that succeeded but was never verified
// (no verification row once the grace period is over, or a verdict of
// unverifiable) is recorded as unverified and earns nothing; while its
// verification may still arrive it stays pending.

func TestClassifyOutcomeNeedsACompletedVerification(t *testing.T) {
	for name, c := range map[string]struct {
		outcome, verification string
		settled               bool
		want                  string
		decided               bool
	}{
		"verified success":             {"success", "success", false, ResultVerifiedRecovery, true},
		"verified success, settled":    {"success", "success", true, ResultVerifiedRecovery, true},
		"no verification yet":          {"success", "", false, "", false},
		"no verification after grace":  {"success", "", true, ResultUnverified, true},
		"unverifiable":                 {"success", "unverifiable", false, ResultUnverified, true},
		"verification pending":         {"success", "pending", true, "", false},
		"verification extended":        {"success", "extended", true, "", false},
		"verification failed":          {"success", "failed", false, ResultNotRecovered, true},
		"action failed":                {"failed", "", false, ResultNotRecovered, true},
		"reverted by verification":     {"success", "revert", false, ResultHarmful, true},
		"rolled back":                  {"rolled_back", "", false, ResultHarmful, true},
		"rollback failed":              {"rollback_failed", "", true, ResultHarmful, true},
		"still executing":              {"pending", "", true, "", false},
		"unknown outcome never credits": {"weird", "success", true, "", false},
	} {
		got, decided := classifyOutcome(c.outcome, c.verification, c.settled)
		if got != c.want || decided != c.decided {
			t.Errorf("%s: classify(%q, %q, settled=%v) = %q %v, want %q %v", name,
				c.outcome, c.verification, c.settled, got, decided, c.want, c.decided)
		}
	}
}

// executedAgo backdates an action's execution.
func (f *fixture) executedAgo(actionLogID int64, ago time.Duration) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, `UPDATE sage.action_log
		SET executed_at = now() - make_interval(secs => $2::double precision)
		WHERE id = $1`, actionLogID, ago.Seconds()); err != nil {
		f.t.Fatal(err)
	}
}

func TestReconcileRecordsUnverifiedOutcomesThatEarnNothing(t *testing.T) {
	f := newReconFixture(t)
	f.cleanMonitored()
	key := "autonomy:wraparound_runway:freeze:public.orders"
	fresh := f.actionLog("success") // verification may still come
	f.handoff(key, fresh)
	stale := f.actionLog("success") // never verified
	f.executedAgo(stale, UnverifiedAfter+time.Minute)
	f.handoff(key, stale)
	unverifiable := f.actionLog("success")
	f.attachVerification(unverifiable, "unverifiable")
	f.handoff(key, unverifiable)
	verified := f.actionLog("success")
	f.attachVerification(verified, "success")
	f.handoff(key, verified)
	res, err := NewReconciler(f.svc, f.pool, f.db, nil).RunOnce(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := f.outcomes(f.db)
	if _, ok := got[fresh]; ok || res.Pending != 1 {
		t.Fatalf("an action inside the grace period was decided: %v (%+v)", got, res)
	}
	if got[stale] != "unverified@L2" || got[unverifiable] != "unverified@L2" ||
		got[verified] != "verified_recovery@L2" || res.Recorded != 3 {
		t.Fatalf("outcomes = %v (%+v)", got, res)
	}
	live, err := f.store.LiveStats(f.ctx, FamilyWraparound, ClassFreeze)
	if err != nil || live.VerifiedL2 != 1 || live.Unverified != 2 || live.HarmfulPair != 0 {
		t.Fatalf("live = %+v (%v): only the verified action earns credit", live, err)
	}
}

// The grace period boundary: just inside it the action is still pending.
func TestReconcileUnverifiedGraceBoundary(t *testing.T) {
	f := newReconFixture(t)
	f.cleanMonitored()
	inside := f.actionLog("success")
	f.executedAgo(inside, UnverifiedAfter-time.Minute)
	f.handoff("autonomy:wraparound_runway:freeze:public.orders", inside)
	res, err := NewReconciler(f.svc, f.pool, f.db, nil).RunOnce(f.ctx)
	if err != nil || res.Pending != 1 || len(f.outcomes(f.db)) != 0 {
		t.Fatalf("inside the grace period = %+v (%v), outcomes %v", res, err,
			f.outcomes(f.db))
	}
}

// Unverified L2 outcomes never reach the L3 bar, however many there are.
func TestUnverifiedOutcomesNeverPromote(t *testing.T) {
	f := newFixture(t)
	f.seedL2Evidence(FamilyWraparound, 25, 0)
	f.promote(FamilyWraparound, ClassFreeze)
	for i := 0; i < 60; i++ {
		if err := f.svc.RecordOutcome(f.ctx, Outcome{Database: f.db,
			ActionLogID: int64(200000 + i), Family: FamilyWraparound, Class: ClassFreeze,
			Level: L2, Result: ResultUnverified, Source: SourceExecutor,
			Actor: ActorPgSage}); err != nil {
			t.Fatal(err)
		}
	}
	created, err := f.svc.ProposePromotions(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range created {
		if p.Family == FamilyWraparound && p.Class == ClassFreeze {
			t.Fatalf("unverified outcomes proposed L3: %+v", p)
		}
	}
	if got := f.granted(FamilyWraparound, ClassFreeze); got != L2 {
		t.Fatalf("an unverified outcome changed the level to %v", got)
	}
}
