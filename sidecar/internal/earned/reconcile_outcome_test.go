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

// earned-01: an executed action that was rolled back earns no credit,
// whenever the reconciler first reads it; the operator's rollback is
// judged on its own (the rollback pass). A failed rollback is harmful.
// Only a regression keeps its own (harmful) result.
func TestClassifyWithVerdictRollbacks(t *testing.T) {
	const unverified, harmful = ResultUnverified, ResultHarmful
	for name, c := range map[string]struct {
		outcome, verification, verdict string
		want                           string
		decided                        bool
	}{
		"rolled back after improving": {"rolled_back", "success", "improved", unverified, true},
		"reverted after improving":    {"reverted", "success", "improved", unverified, true},
		"rolled back, insufficient": {"rolled_back", "", "insufficient_evidence", unverified,
			true},
		"rolled back, unverifiable":    {"rolled_back", "", "unverifiable", unverified, true},
		"neutral index-create revert":  {"rolled_back", "revert", "neutral", unverified, true},
		"rolled back after regressing": {"rolled_back", "revert", "regressed", harmful, true},
		"reverted after regressing":    {"reverted", "", "regressed", harmful, true},
		"rollback failed, neutral":     {"rollback_failed", "", "neutral", harmful, true},
		"rollback failed, improved":    {"rollback_failed", "success", "improved", harmful, true},
		"rollback failed, regressed":   {"rollback_failed", "", "regressed", harmful, true},
		"rolled back, verdict pending": {"rolled_back", "", "pending", "", false},
		"rolled back, unknown verdict": {"rolled_back", "success", "great", "", false},
		"kept improvement still earns": {"success", "success", "improved", ResultVerifiedRecovery,
			true},
	} {
		for _, settled := range []bool{false, true} {
			got, decided := classifyWithVerdict(c.outcome, c.verification, c.verdict, settled)
			if got != c.want || decided != c.decided {
				t.Errorf("%s (settled %v): classifyWithVerdict(%q, %q, %q) = %q %v, want %q %v",
					name, settled, c.outcome, c.verification, c.verdict, got, decided, c.want,
					c.decided)
			}
		}
	}
}

// The legacy rules (no verdict) are unchanged: a rolled-back action
// without a verdict is still harmful.
func TestClassifyWithVerdictLegacyRollbackUnchanged(t *testing.T) {
	for _, outcome := range []string{"rolled_back", "reverted", "rollback_failed"} {
		got, decided := classifyWithVerdict(outcome, "", "", true)
		if got != ResultHarmful || !decided {
			t.Errorf("legacy %s = %q %v, want harmful", outcome, got, decided)
		}
	}
}

// A rolled-back action is judged under an incident pair when its decision
// names one, else when its approval item is an autonomy handoff, else
// under its self-initiated class. A malformed incident marker is never
// re-read as a self-initiated class.
func TestRollbackRowPair(t *testing.T) {
	str := func(s string) *string { return &s }
	for name, c := range map[string]struct {
		row    rollbackRow
		family Family
		class  ActionClass
		level  Level
		ok     bool
	}{
		"L3 incident action": {rollbackRow{incidentFamily: str("wraparound_runway"),
			incidentClass: "freeze", reason: "autonomy_l3", outcomeClass: "vacuum"},
			FamilyWraparound, ClassFreeze, L3, true},
		"deadline override": {rollbackRow{incidentFamily: str("wraparound_runway"),
			incidentClass: "freeze", reason: "deadline_override"},
			FamilyWraparound, ClassFreeze, L1, true},
		"handoff without decision": {rollbackRow{outcomeClass: "vacuum",
			label: "vacuum", handoffKey: "autonomy:wal_retention:wal_bound:slot:s1"},
			FamilyWAL, ClassWALBound, L2, true},
		"incident decision of a handoff": {rollbackRow{incidentFamily: str("wal_retention"),
			incidentClass: "wal_bound", reason: "operator_approved",
			handoffKey: "autonomy:wal_retention:wal_bound:slot:s1"},
			FamilyWAL, ClassWALBound, L2, true},
		"unknown incident family": {rollbackRow{incidentFamily: str("shell"),
			incidentClass: "freeze", outcomeClass: "vacuum"}, "", "", L1, false},
		"incident family, no class": {rollbackRow{incidentFamily: str("wraparound_runway"),
			outcomeClass: "vacuum"}, "", "", L1, false},
		"empty incident family": {rollbackRow{incidentFamily: str(""), label: "vacuum"},
			"", "", L1, false},
		"malformed handoff key": {rollbackRow{handoffKey: "autonomy:bogus",
			outcomeClass: "vacuum"}, "", "", L2, false},
		"self-initiated by outcome class": {rollbackRow{outcomeClass: "guc",
			reason: "autonomy_l3"}, FamilyTuning, ClassConfigGUC, L3, true},
		"self-initiated by label": {rollbackRow{label: "drop_index", reason: "authorized"},
			FamilyHygiene, ClassIndexDrop, L1, true},
		"approved self-initiated": {rollbackRow{outcomeClass: "vacuum", approved: true},
			FamilyHygiene, ClassVacuum, L2, true},
		"nothing to judge": {rollbackRow{label: "select"}, "", "", L1, false},
		"zero row":         {rollbackRow{}, "", "", L1, false},
	} {
		f, cl, level, ok := c.row.pair()
		if ok != c.ok || (ok && (f != c.family || cl != c.class || level != c.level)) {
			t.Errorf("%s: pair() = %s/%s L%d %v, want %s/%s L%d %v", name, f, cl, level, ok,
				c.family, c.class, c.level, c.ok)
		}
	}
}

// The no-gain revert of a neutral index create is no demerit in an
// incident family either; an operator's rollback of an improved one is.
func TestRollbackIsDemeritForIncidentClasses(t *testing.T) {
	for _, c := range []struct {
		class   ActionClass
		verdict string
		want    bool
	}{
		{ClassIndexCreate, "neutral", false},
		{ClassIndexCreate, "improved", true},
		{ClassFreeze, "neutral", true},
		{ClassFreeze, "improved", true},
		{ClassFreeze, "regressed", false},
		{ClassFreeze, "", true},
		{ClassWALBound, "pending", true},
	} {
		if got := rollbackIsDemerit(c.class, c.verdict); got != c.want {
			t.Errorf("rollbackIsDemerit(%s, %q) = %v, want %v", c.class, c.verdict, got, c.want)
		}
	}
}
