package policy

import (
	"context"
	"strings"
	"testing"
	"time"
)

// executor-01: the soft-drop restore of pg_sage's own autonomous drop is a
// condition of the autonomy the drop ran under. A trust downgrade must not
// strand it; only the hard stops (and the policy document) still bind.

const restoreSQL = `CREATE INDEX CONCURRENTLY orders_status_idx ON public.orders ` +
	`USING btree (status)`

// dropRestoreRequest is the request authorizeDroppedIndexRestore sends: the
// restore contract, marked a rollback, aimed at the dropped index's table.
func dropRestoreRequest() ActionRequest {
	return ActionRequest{
		Contract: &ActionContract{ActionType: ActionRestoreDroppedIndex, RiskTier: RiskSafe,
			RollbackClass: RollbackReversible},
		SQL: restoreSQL, TargetObjs: []string{"public.orders"}, Feature: "index",
		Rollback: true,
	}
}

func restoreDecision(t *testing.T, runtime RuntimeState, req ActionRequest) Decision {
	t.Helper()
	gate := newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true, now: safetyNow})
	return gate.Authorize(context.Background(), req)
}

func TestDropRestoreExecutesAtAutonomousTrust(t *testing.T) {
	d := restoreDecision(t, enabledRuntime(), dropRestoreRequest())

	assertDecision(t, d, VerdictExecute, ReasonDropRestore)
	if d.RiskTier != RiskSafe {
		t.Fatalf("risk tier = %q, want safe", d.RiskTier)
	}
	if d.LockCeilingMS != 3000 {
		t.Fatalf("lock ceiling = %d, want the document's 3000 ms bound", d.LockCeilingMS)
	}
}

// One test per trust downgrade (state transitions): each would hold a
// fresh safe action back, and none strands the restore. The decision names
// the bypass and which downgrade it passed.
func TestDropRestoreSurvivesEveryTrustDowngrade(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*RuntimeState)
		fresh   Verdict
		detail  string
		freshRe Reason
	}{
		{"observation trust", func(r *RuntimeState) { r.TrustLevel = TrustObservation },
			VerdictObserveOnly, "trust observation", ReasonObserveOnly},
		{"approval mode", func(r *RuntimeState) { r.ExecutionMode = ExecutionApproval },
			VerdictQueueApproval, "execution_mode approval", ReasonApprovalRequired},
		{"manual mode", func(r *RuntimeState) { r.ExecutionMode = ExecutionManual },
			VerdictObserveOnly, "execution_mode manual", ReasonObserveOnly},
		{"tier3_safe off", func(r *RuntimeState) { r.Tier3Safe = false },
			VerdictBlocked, "tier3_safe off", ReasonTrustRampNotSatisfied},
		{"ramp reset", func(r *RuntimeState) { r.RampStart = safetyNow.Add(-time.Minute) },
			VerdictBlocked, "trust ramp not satisfied", ReasonTrustRampNotSatisfied},
		{"ramp never started", func(r *RuntimeState) { r.RampStart = time.Time{} },
			VerdictBlocked, "trust ramp not satisfied", ReasonTrustRampNotSatisfied},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runtime := enabledRuntime()
			c.mutate(&runtime)
			fresh := dropRestoreRequest()
			fresh.Rollback = false
			assertDecision(t, restoreDecision(t, runtime, fresh), c.fresh, c.freshRe)

			d := restoreDecision(t, runtime, dropRestoreRequest())

			assertDecision(t, d, VerdictExecute, ReasonDropRestore)
			if !strings.Contains(d.Detail, c.detail) {
				t.Fatalf("detail = %q, want it to name %q", d.Detail, c.detail)
			}
		})
	}
}

// With no downgrade in force the detail still says what the decision is,
// and claims no bypassed downgrade.
func TestDropRestoreDetailWithoutDowngrade(t *testing.T) {
	d := restoreDecision(t, enabledRuntime(), dropRestoreRequest())
	if !strings.Contains(d.Detail, "restore of an index pg_sage dropped") ||
		strings.Contains(d.Detail, "bypassed") {
		t.Fatalf("detail = %q, want the restore named and no bypass claimed", d.Detail)
	}
}

// The hard stops still bind: a stop halts everything.
func TestDropRestoreStillStopsOnHardStops(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*RuntimeState)
		reason Reason
	}{
		{"emergency stop", func(r *RuntimeState) { r.EmergencyStop = true },
			ReasonEmergencyStop},
		{"executor disabled", func(r *RuntimeState) { r.ExecutorEnabled = false },
			ReasonExecutorDisabled},
		{"replica", func(r *RuntimeState) { r.IsReplica = true }, ReasonReplicaMutation},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runtime := enabledRuntime()
			runtime.TrustLevel = TrustObservation
			c.mutate(&runtime)
			assertDecision(t, restoreDecision(t, runtime, dropRestoreRequest()),
				VerdictBlocked, c.reason)
		})
	}
}

// Invalid input: no other rollback gains the bypass, and the restore
// contract without the rollback mark is an ordinary request.
func TestOnlyTheDropRestoreBypassesObservation(t *testing.T) {
	runtime := enabledRuntime()
	runtime.TrustLevel = TrustObservation
	for _, actionType := range []string{"revert_created_index", "set_table_autovacuum",
		"create_index_concurrently", "drop_unused_index"} {
		req := dropRestoreRequest()
		req.Contract.ActionType = actionType
		assertDecision(t, restoreDecision(t, runtime, req), VerdictObserveOnly,
			ReasonObserveOnly)
	}
	if IsDropRestore(ActionRequest{Rollback: true}) {
		t.Fatal("a rollback without a contract is a drop restore")
	}
	forward := dropRestoreRequest()
	forward.Rollback = false
	if IsDropRestore(forward) {
		t.Fatal("the restore contract without the rollback mark is a drop restore")
	}
}

// Invalid input: an unknown trust level fails closed even for a restore.
func TestDropRestoreUnknownTrustLevelFailsClosed(t *testing.T) {
	runtime := enabledRuntime()
	runtime.TrustLevel = "superuser"
	d := restoreDecision(t, runtime, dropRestoreRequest())
	assertDecision(t, d, VerdictBlocked, ReasonUnknownTrustLevel)
}

// The standing policy document still binds: a class the owner removed
// blocks, an approval class queues, a degraded build queues.
func TestDropRestoreStillBoundByTheDocument(t *testing.T) {
	doc := UnattendedProfile()
	doc.AllowedChangeClasses = []ChangeClass{ChangeVacuum}
	gate := newTestGate(t, gateFixture{policy: doc, now: safetyNow})
	assertDecision(t, gate.Authorize(context.Background(), dropRestoreRequest()),
		VerdictBlocked, ReasonChangeClassNotAllowed)

	doc = UnattendedProfile()
	doc.ApprovalRequiredClasses = append(doc.ApprovalRequiredClasses, ChangeIndex)
	gate = newTestGate(t, gateFixture{policy: doc, now: safetyNow})
	assertDecision(t, gate.Authorize(context.Background(), dropRestoreRequest()),
		VerdictQueueApproval, ReasonApprovalRequired)

	runtime := enabledRuntime()
	runtime.SQLValidationDegraded = true
	assertDecision(t, restoreDecision(t, runtime, dropRestoreRequest()),
		VerdictQueueApproval, ReasonSQLValidationDegraded)
}

// Error propagation: an unreadable policy blocks the restore with the
// read error, it is never executed blind.
func TestDropRestoreFailsClosedWithoutPolicy(t *testing.T) {
	gate := newTestGate(t, gateFixture{policyErr: ErrPolicyNotFound, now: safetyNow,
		policy: Document{Profile: ProfileUnattended}})
	d := gate.Authorize(context.Background(), dropRestoreRequest())
	assertDecision(t, d, VerdictBlocked, ReasonPolicyUnavailable)
}

// The restore never counts toward a kind budget, with or without the
// rollback mark (it only re-creates what existed).
func TestDropRestoreBypassesTheBudgets(t *testing.T) {
	forward := dropRestoreRequest()
	forward.Rollback = false
	if got := BudgetBypassFor(forward, safetyNow); !strings.Contains(got, "pg_sage dropped") {
		t.Fatalf("bypass = %q, want the restore named", got)
	}
	doc := UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	doc.BlastRadius.MaxTablesPerWindow = 1
	doc.RateLimits.MaxSelfInitiatedChangesPerWindow = 1
	full := LimitUsage{TablesInWindow: 5, SelfInitiatedChangesInWindow: 5}
	gate := newTestGate(t, gateFixture{policy: doc, usage: full, now: safetyNow})
	if d := gate.Authorize(context.Background(), validIndexRequest()); d.Verdict != VerdictPark {
		t.Fatalf("fresh create on a full budget = %#v, want parked", d)
	}
	d := gate.Authorize(context.Background(), dropRestoreRequest())
	assertDecision(t, d, VerdictExecute, ReasonDropRestore)
	if d.BudgetKind != BudgetBypass {
		t.Fatalf("budget kind = %q, want %q", d.BudgetKind, BudgetBypass)
	}
}

// Explain gives the same verdict as Authorize and records nothing.
func TestDropRestoreExplainMatchesAuthorize(t *testing.T) {
	runtime := enabledRuntime()
	runtime.TrustLevel = TrustObservation
	calls := []string{}
	gate := newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true, now: safetyNow,
		calls: &calls})
	explainer, ok := gate.(Explainer)
	if !ok {
		t.Fatal("the gate does not explain")
	}
	d := explainer.Explain(context.Background(), dropRestoreRequest())
	if d.Verdict != VerdictExecute || d.Reason != ReasonDropRestore {
		t.Fatalf("explain = %#v, want execute by the restore bypass", d)
	}
	assertNotCalled(t, calls, "evidence")
}

// No concurrent access test here: the restore decision reads only its
// request and the runtime snapshot; the gate's shared budget section is
// covered by the budget race tests.
