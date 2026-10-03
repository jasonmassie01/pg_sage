package earned

import (
	"testing"
)

// Phase 1.3: when an action carries a predicted-vs-observed verdict
// (sage.action_outcome), the verdict decides the ledger result. Only an
// improved verdict is a verified recovery; neutral, insufficient
// evidence and unverifiable earn nothing and cost nothing; regressed is
// harmful. Rows without a verdict keep the P0-6 rules.

func TestClassifyWithVerdict(t *testing.T) {
	for name, c := range map[string]struct {
		outcome, verification, verdict string
		settled                        bool
		want                           string
		decided                        bool
	}{
		"improved":     {"success", "success", "improved", true, ResultVerifiedRecovery, true},
		"neutral kept": {"success", "unverifiable", "neutral", true, ResultUnverified, true},
		"neutral overrides success verification": {"success", "success", "neutral", true,
			ResultUnverified, true},
		"insufficient": {"unverifiable", "unverifiable", "insufficient_evidence", true,
			ResultUnverified, true},
		"unverifiable": {"unverifiable", "unverifiable", "unverifiable", false,
			ResultUnverified, true},
		"regressed": {"rolled_back", "revert", "regressed", true, ResultHarmful, true},
		"regressed, rollback withheld": {"rollback_skipped", "", "regressed", true,
			ResultHarmful, true},
		"neutral create reverted for no gain": {"rolled_back", "revert", "neutral", true,
			ResultUnverified, true},
		"pending verdict waits": {"monitoring", "", "pending", true, "", false},
		"failed action":         {"failed", "failed", "improved", true, ResultNotRecovered, true},
		"legacy row, verified":  {"success", "success", "", true, ResultVerifiedRecovery, true},
		"legacy row, unverified after grace": {"success", "", "", true, ResultUnverified,
			true},
		"unknown verdict never credits": {"success", "success", "great", true, "", false},
	} {
		got, decided := classifyWithVerdict(c.outcome, c.verification, c.verdict, c.settled)
		if got != c.want || decided != c.decided {
			t.Errorf("%s: classifyWithVerdict(%q, %q, %q, %v) = %q %v, want %q %v", name,
				c.outcome, c.verification, c.verdict, c.settled, got, decided, c.want,
				c.decided)
		}
	}
}

// The reconciler reads the verdict: a neutral action with a "success"
// verification (index create kept) earns nothing; an improved one earns.
func TestReconcileUsesOutcomeVerdict(t *testing.T) {
	f := newReconFixture(t)
	f.cleanMonitored()
	key := "autonomy:wraparound_runway:freeze:public.orders"
	neutral := f.actionLog("success")
	f.attachVerification(neutral, "success")
	f.attachOutcome(neutral, "neutral")
	f.handoff(key, neutral)
	improved := f.actionLog("success")
	f.attachVerification(improved, "success")
	f.attachOutcome(improved, "improved")
	f.handoff(key, improved)
	regressed := f.actionLog("rollback_skipped")
	f.attachOutcome(regressed, "regressed")
	f.handoff(key, regressed)

	if _, err := NewReconciler(f.svc, f.pool, f.db, nil).RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	got := f.outcomes(f.db)
	if got[neutral] != "unverified@L2" || got[improved] != "verified_recovery@L2" ||
		got[regressed] != "harmful@L2" {
		t.Fatalf("outcomes = %v", got)
	}
}

func (f *fixture) attachOutcome(actionLogID int64, verdict string) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO sage.action_outcome
		(action_log_id, action_class, predicted, prediction_method, verdict, tolerance,
		 decided_at)
		VALUES ($1, 'vacuum', '{"method":"rule"}', 'rule', $2, 'met', now())`,
		actionLogID, verdict); err != nil {
		f.t.Fatalf("action_outcome: %v", err)
	}
}
