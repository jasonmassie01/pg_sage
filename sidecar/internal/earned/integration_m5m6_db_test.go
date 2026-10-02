package earned

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/policy"
)

// M7 on the integrated M5+M6 code (coordinator, 2026-10-02):
//   - M5's cancel_backend is a mitigation-only, approval-only class:
//     never above L2, never carried over, and its verified recoveries
//     are promotion evidence for its family;
//   - every family action the gate let execute is a live outcome: an L3
//     auto-execution (notified) and a mandatory deadline override
//     (recorded at L1, never as promotion evidence, but a harmful one is a
//     family safety regression);
//   - a runbook's typed action names its autonomy class.

func TestClassForActionType(t *testing.T) {
	for actionType, want := range map[string]ActionClass{
		"cancel_backend": ClassBackendCancel, "terminate_backend": ClassBackendTerminate,
		"vacuum_table": ClassVacuum, "analyze_table": ClassAnalyze,
		"create_statistics":                   ClassStatistics,
		"prepare_sequence_capacity_migration": ClassSequenceMigration,
		"diagnose_lock_blockers":              "", "investigate_query_plan": "",
		"": "", "drop_database": "",
	} {
		if got := ClassForActionType(actionType); got != want {
			t.Errorf("ClassForActionType(%q) = %q, want %q", actionType, got, want)
		}
	}
}

func cancelRequest() policy.ActionRequest {
	return policy.ActionRequest{IncidentFamily: string(FamilyLockBlocking),
		Feature: "backend_signal", SQL: "SELECT pg_cancel_backend(4242)",
		TargetObjs: []string{"pid:4242"},
		Contract: &policy.ActionContract{ActionType: "cancel_backend",
			RiskTier: policy.RiskModerate, RollbackClass: policy.RollbackMitigationOnly}}
}

func TestCancelBackendIsApprovalOnly(t *testing.T) {
	if c := ClassFor(cancelRequest()); c != ClassBackendCancel {
		t.Fatalf("class = %q", c)
	}
	if CapFor(ClassBackendCancel) != L2 || RollbackCap(policy.RollbackMitigationOnly) != L2 {
		t.Fatal("cancel_backend must be capped at L2")
	}
	for _, f := range []Family{FamilyLockBlocking, FamilyConnections} {
		if CarriedLevel(autonomousBound(), f, ClassBackendCancel) != L1 {
			t.Fatalf("%s/backend_cancel is carried over", f)
		}
	}
	lf := newLimiterFixture(t)
	// Even a ledger row forced to L3 behind the service's back acts at L2.
	if _, err := lf.pool.Exec(lf.ctx, `INSERT INTO sage.sre_family_autonomy
		(deployment_id, family, action_class, level, changed_by, change_reason)
		VALUES ($1, 'lock_blocking', 'backend_cancel', 3, 'test', 'forced')`,
		lf.store.DeploymentID()); err != nil {
		t.Fatal(err)
	}
	req := cancelRequest()
	req.EvidenceObservedAt = lf.clock.Now()
	if got := lf.limit(req); got.Level > 2 {
		t.Fatalf("forced L3 cancel limit = %+v, want at most L2", got)
	}
}

func TestCancelRecoveriesAreFamilyEvidence(t *testing.T) {
	f := newFixture(t)
	for i, result := range []string{ResultVerifiedRecovery, ResultVerifiedRecovery,
		ResultNotRecovered} {
		if err := f.svc.RecordOutcome(f.ctx, Outcome{Database: "orders",
			ActionLogID: int64(7000 + i), Family: FamilyLockBlocking,
			Class: ClassBackendCancel, Level: L2, Result: result,
			Source: SourceExecutor, Actor: ActorPgSage}); err != nil {
			t.Fatal(err)
		}
	}
	ev, err := f.svc.Evidence(f.ctx, FamilyLockBlocking, ClassBackendCancel)
	if err != nil || ev.Live.VerifiedL2 != 2 || ev.Live.HarmfulPair != 0 {
		t.Fatalf("live evidence = %+v (%v)", ev.Live, err)
	}
}

func (f *fixture) familyDecision(reason, family, class, outcome string) int64 {
	f.t.Helper()
	var decision int64
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.decision
		(feature, intent, target_objects, verdict, risk_tier, reason, evidence, evidence_id)
		VALUES ('freeze', 'freeze', '["public.orders"]', 'execute', 'safe', $1,
		        CASE WHEN $2::text = '' THEN '{}'::jsonb ELSE
		        jsonb_build_object('incident_family', $2::text, 'autonomy_class', $3::text)
		        END, md5(random()::text)) RETURNING id`, reason, family, class).
		Scan(&decision); err != nil {
		f.t.Fatalf("decision: %v", err)
	}
	id := f.actionLog(outcome)
	if _, err := f.pool.Exec(f.ctx, `UPDATE sage.action_log SET decision_id = $1
		WHERE id = $2`, decision, id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func TestReconcileRecordsMandatoryDeadlineExecutions(t *testing.T) {
	f := newFixture(t)
	f.cleanMonitored()
	db := "deadline-" + newUUID(t)[:8]
	override := f.familyDecision("deadline_override", "wraparound_runway", "freeze",
		"success")
	inWindow := f.familyDecision("authorized", "wraparound_runway", "freeze", "success")
	plain := f.familyDecision("authorized", "", "", "success")
	notifier := &recordingNotifier{}
	res, err := NewReconciler(f.svc, f.pool, db, notifier).RunOnce(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := f.outcomes(db)
	if got[override] != "verified_recovery@L1" || got[inWindow] != "verified_recovery@L1" ||
		got[plain] != "" || res.Recorded != 2 {
		t.Fatalf("outcomes = %v, result = %+v", got, res)
	}
	if len(notifier.seen) != 0 {
		t.Fatalf("a mandatory deadline action was notified as L3: %+v", notifier.seen)
	}
	ev, err := f.svc.Evidence(f.ctx, FamilyWraparound, ClassFreeze)
	if err != nil || ev.Live.VerifiedL2 != 0 {
		t.Fatalf("deadline overrides counted as promotion evidence: %+v (%v)", ev.Live, err)
	}
}

func TestHarmfulDeadlineExecutionIsAFamilyRegression(t *testing.T) {
	f := newFixture(t)
	f.cleanMonitored()
	f.seedL2Evidence(FamilyWraparound, 25, 0)
	f.promote(FamilyWraparound, ClassVacuum)
	db := "deadline-" + newUUID(t)[:8]
	harmful := f.familyDecision("deadline_override", "wraparound_runway", "freeze",
		"rolled_back")
	if _, err := NewReconciler(f.svc, f.pool, db, nil).RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.outcomes(db); got[harmful] != "harmful@L1" {
		t.Fatalf("outcomes = %v", got)
	}
	if f.granted(FamilyWraparound, ClassVacuum) != L1 {
		t.Fatal("a harmful deadline execution did not demote the family's earned pairs")
	}
}

// The M6 families (reactive and runways) are in the ledger and its view
// with the classes that can remediate them.
func TestViewListsTheM6Families(t *testing.T) {
	f := newFixture(t)
	v, err := f.svc.View(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[Family]int{}
	for _, fv := range v.Families {
		got[fv.Family] = len(fv.Classes)
	}
	for _, fam := range []Family{FamilyCheckpoint, FamilyTempFiles, FamilyReplicationLag,
		FamilyLWLock, FamilyWraparound, FamilyDiskWAL, FamilySequence} {
		if got[fam] == 0 || got[fam] != len(ApplicableClasses(fam)) {
			t.Errorf("view family %s has %d classes, want %d", fam, got[fam],
				len(ApplicableClasses(fam)))
		}
	}
	if len(v.Families) != 11 {
		t.Fatalf("view lists %d families, want 11", len(v.Families))
	}
}

// Post-test audit (mutation I12 survived): an operator-approved execution
// of a family action is the approval path's (an L2 handoff), never a
// self-initiated outcome.
func TestReconcileLeavesOperatorApprovalsToTheHandoffPath(t *testing.T) {
	f := newFixture(t)
	f.cleanMonitored()
	db := "approved-" + newUUID(t)[:8]
	approved := f.familyDecision("operator_approved", "wraparound_runway", "freeze",
		"success")
	res, err := NewReconciler(f.svc, f.pool, db, nil).RunOnce(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.outcomes(db); got[approved] != "" || res.Recorded != 0 {
		t.Fatalf("an operator approval was recorded as self-initiated: %v", got)
	}
}
