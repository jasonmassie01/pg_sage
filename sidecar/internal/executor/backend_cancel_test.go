package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Sage SRE M5: an approved, evidence-matched cancel of one backend. The
// identity is pid + backend_start + database + user + query hash (plus
// query_start and query_id), the evidence must be at most 5 s old when
// the signal is sent, and the signal SQL rechecks the identity in the
// same statement (CHECK-18, CHECK-19).

func advisoryConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "advisory"
	return cfg
}

// cancelExecutor is an executor over pool whose gate answers verdict.
func cancelExecutor(pool *pgxpool.Pool, verdict policy.Decision) (*Executor,
	*custodianGateCapture) {
	exec := New(pool, advisoryConfig(), time.Time{}, nopLog)
	exec.emergencyStopFn = func(context.Context) bool { return false }
	gate := &custodianGateCapture{verdict: verdict}
	exec.WithPolicyGate(gate)
	return exec, gate
}

func executeVerdict() policy.Decision {
	return policy.Decision{Verdict: policy.VerdictExecute, RiskTier: policy.RiskModerate,
		Reason: policy.ReasonOperatorApproved, DecisionID: 0}
}

// sleeperIdentity reads the full identity of a sleeping backend.
func sleeperIdentity(t *testing.T, pool *pgxpool.Pool, b sleepingBackend) BackendIdentity {
	t.Helper()
	id := BackendIdentity{PID: int32(b.pid), BackendStart: b.backendStart,
		QueryStart: b.queryStart, QueryID: b.queryID}
	err := pool.QueryRow(context.Background(), `SELECT datname, usename,
		encode(sha256(convert_to(query, 'UTF8')), 'hex')
		FROM pg_stat_activity WHERE pid = $1`, b.pid).
		Scan(&id.Database, &id.User, &id.QueryHash)
	if err != nil {
		t.Fatalf("read sleeper identity: %v", err)
	}
	return id
}

func validIdentity() BackendIdentity {
	return BackendIdentity{PID: 4242, BackendStart: time.Now().Add(-time.Hour),
		QueryStart: time.Now().Add(-time.Minute), Database: "orders", User: "app",
		QueryHash: strings.Repeat("ab", 32), QueryID: 77}
}

func TestBackendIdentityValidate(t *testing.T) {
	if err := validIdentity().Validate(); err != nil {
		t.Fatalf("valid identity: %v", err)
	}
	for name, mutate := range map[string]func(*BackendIdentity){
		"zero pid":         func(b *BackendIdentity) { b.PID = 0 },
		"negative pid":     func(b *BackendIdentity) { b.PID = -1 },
		"no backend_start": func(b *BackendIdentity) { b.BackendStart = time.Time{} },
		"no query_start":   func(b *BackendIdentity) { b.QueryStart = time.Time{} },
		"no database":      func(b *BackendIdentity) { b.Database = " " },
		"no user":          func(b *BackendIdentity) { b.User = "" },
		"short hash":       func(b *BackendIdentity) { b.QueryHash = "abc" },
		"non-hex hash":     func(b *BackendIdentity) { b.QueryHash = strings.Repeat("zz", 32) },
		"query before start": func(b *BackendIdentity) {
			b.QueryStart = b.BackendStart.Add(-time.Second)
		},
	} {
		b := validIdentity()
		mutate(&b)
		if err := b.Validate(); !errors.Is(err, ErrInvalidBackendCancel) {
			t.Errorf("%s: Validate = %v, want ErrInvalidBackendCancel", name, err)
		}
	}
}

func TestCancelBackendRequiresApprovalBeforeTheGate(t *testing.T) {
	exec, gate := cancelExecutor(nil, executeVerdict())
	_, err := exec.CancelBackend(context.Background(), BackendCancel{
		Target: validIdentity(), ObservedAt: time.Now(), FindingID: 1})
	if !errors.Is(err, ErrBackendApprovalRequired) {
		t.Fatalf("unapproved cancel = %v, want ErrBackendApprovalRequired", err)
	}
	if gate.calls != 0 {
		t.Fatalf("gate called %d times for an unapproved cancel", gate.calls)
	}
}

func TestCancelBackendRejectsInvalidRequests(t *testing.T) {
	exec, gate := cancelExecutor(nil, executeVerdict())
	for name, req := range map[string]BackendCancel{
		"loose evidence age": {Target: validIdentity(), ObservedAt: time.Now(),
			MaxEvidenceAge: 6 * time.Second, FindingID: 1, ApprovedBy: 1},
		"negative evidence age": {Target: validIdentity(), ObservedAt: time.Now(),
			MaxEvidenceAge: -time.Second, FindingID: 1, ApprovedBy: 1},
		"no observation time": {Target: validIdentity(), FindingID: 1, ApprovedBy: 1},
		"bad identity": {Target: BackendIdentity{}, ObservedAt: time.Now(),
			FindingID: 1, ApprovedBy: 1},
		"no finding": {Target: validIdentity(), ObservedAt: time.Now(), ApprovedBy: 1},
	} {
		if _, err := exec.CancelBackend(context.Background(), req); !errors.Is(err,
			ErrInvalidBackendCancel) {
			t.Errorf("%s: CancelBackend = %v, want ErrInvalidBackendCancel", name, err)
		}
	}
	if gate.calls != 0 {
		t.Fatalf("gate called %d times for invalid requests", gate.calls)
	}
}

func TestCancelBackendGateRequestShape(t *testing.T) {
	exec, gate := cancelExecutor(nil, policy.Decision{Verdict: policy.VerdictBlocked,
		Reason: policy.ReasonEmergencyStop})
	target := validIdentity()
	_, err := exec.CancelBackend(context.Background(), BackendCancel{Target: target,
		ObservedAt: time.Now(), FindingID: 9, ApprovedBy: 7, IsReplica: true,
		Evidence: map[string]any{"investigation_id": "inv-1", "proposal_id": "p-1"}})
	var withheld *WithheldError
	if !errors.As(err, &withheld) || !errors.Is(err, ErrActionWithheld) {
		t.Fatalf("blocked cancel = %v, want *WithheldError", err)
	}
	req := gate.request
	if !req.OperatorApproved || req.Contract == nil ||
		req.Contract.ActionType != "cancel_backend" ||
		req.Contract.RollbackClass != policy.RollbackMitigationOnly ||
		req.Contract.RiskTier != policy.RiskModerate {
		t.Fatalf("gate request contract = %+v (approved=%v)", req.Contract,
			req.OperatorApproved)
	}
	if req.Feature != string(policy.ChangeBackendSignal) || !req.IsReplica ||
		req.SQL != "SELECT pg_cancel_backend(4242)" ||
		len(req.TargetObjs) != 1 || req.TargetObjs[0] != "pid:4242" {
		t.Fatalf("gate request = %+v", req)
	}
	if req.Evidence["approved_by"] != 7 || req.Evidence["proposal_id"] != "p-1" ||
		req.Evidence["investigation_id"] != "inv-1" || req.Evidence["finding_id"] != 9 {
		t.Fatalf("gate evidence = %#v", req.Evidence)
	}
}

func TestCancelBackendSignalsExactIdentityAndRecordsIt(t *testing.T) {
	pool, ctx := requireDB(t)
	backend := startSleepingBackend(t, pool, 10)
	exec, gate := cancelExecutor(pool, executeVerdict())
	findingID := insertCancelFinding(t, pool)

	actionID, err := exec.CancelBackend(ctx, BackendCancel{
		Target: sleeperIdentity(t, pool, backend), ObservedAt: time.Now(),
		FindingID: findingID, ApprovedBy: 3})
	if err != nil {
		t.Fatalf("CancelBackend exact identity: %v", err)
	}
	backend.waitCancelled(t)
	if gate.calls != 2 {
		t.Fatalf("gate calls = %d, want authorize and re-authorize", gate.calls)
	}
	var actionType, sqlText, outcome string
	var approvedBy int
	err = pool.QueryRow(ctx, `SELECT action_type, sql_executed, outcome, approved_by
		FROM sage.action_log WHERE id = $1`, actionID).
		Scan(&actionType, &sqlText, &outcome, &approvedBy)
	if err != nil {
		t.Fatalf("action_log row %d: %v", actionID, err)
	}
	want := fmt.Sprintf("SELECT pg_cancel_backend(%d)", backend.pid)
	if actionType != "cancel_backend" || sqlText != want || outcome != "success" ||
		approvedBy != 3 {
		t.Fatalf("action_log = %s %q %s by %d", actionType, sqlText, outcome, approvedBy)
	}
}

func TestCancelBackendRefusesStaleEvidence(t *testing.T) {
	pool, ctx := requireDB(t)
	backend := startSleepingBackend(t, pool, 2)
	exec, _ := cancelExecutor(pool, executeVerdict())
	_, err := exec.CancelBackend(ctx, BackendCancel{
		Target:     sleeperIdentity(t, pool, backend),
		ObservedAt: time.Now().Add(-6 * time.Second), FindingID: 1, ApprovedBy: 3})
	if !errors.Is(err, ErrBackendEvidenceStale) {
		t.Fatalf("6 s old evidence = %v, want ErrBackendEvidenceStale", err)
	}
	backend.waitUncancelled(t)
}

func TestCancelBackendTighterEvidenceAgeHolds(t *testing.T) {
	pool, ctx := requireDB(t)
	backend := startSleepingBackend(t, pool, 2)
	exec, _ := cancelExecutor(pool, executeVerdict())
	_, err := exec.CancelBackend(ctx, BackendCancel{
		Target:     sleeperIdentity(t, pool, backend),
		ObservedAt: time.Now().Add(-2 * time.Second), MaxEvidenceAge: time.Second,
		FindingID: 1, ApprovedBy: 3})
	if !errors.Is(err, ErrBackendEvidenceStale) {
		t.Fatalf("2 s old evidence under a 1 s bound = %v, want stale", err)
	}
	backend.waitUncancelled(t)
}

// CHECK-19: a reused pid, a new query on the same session, another user or
// database, or a different query text never matches.
func TestCancelBackendRefusesChangedIdentity(t *testing.T) {
	pool, ctx := requireDB(t)
	for name, mutate := range map[string]func(*BackendIdentity){
		"reused pid":     func(b *BackendIdentity) { b.BackendStart = b.BackendStart.Add(-time.Hour) },
		"new query":      func(b *BackendIdentity) { b.QueryStart = b.QueryStart.Add(time.Millisecond) },
		"other user":     func(b *BackendIdentity) { b.User = "somebody_else" },
		"other database": func(b *BackendIdentity) { b.Database = "other_db" },
		"other text":     func(b *BackendIdentity) { b.QueryHash = strings.Repeat("0", 64) },
		"other query id": func(b *BackendIdentity) { b.QueryID++ },
	} {
		t.Run(name, func(t *testing.T) {
			backend := startSleepingBackend(t, pool, 2)
			exec, _ := cancelExecutor(pool, executeVerdict())
			target := sleeperIdentity(t, pool, backend)
			mutate(&target)
			_, err := exec.CancelBackend(ctx, BackendCancel{Target: target,
				ObservedAt: time.Now(), FindingID: 1, ApprovedBy: 3})
			if !errors.Is(err, ErrBackendEvidenceStale) {
				t.Fatalf("changed identity = %v, want ErrBackendEvidenceStale", err)
			}
			backend.waitUncancelled(t)
		})
	}
}

func TestCancelBackendNeverSignalsProtectedRoles(t *testing.T) {
	pool, ctx := requireDB(t)
	backend := startSleepingBackend(t, pool, 2)
	exec, _ := cancelExecutor(pool, executeVerdict())
	target := sleeperIdentity(t, pool, backend)
	_, err := exec.CancelBackend(ctx, BackendCancel{Target: target,
		ObservedAt: time.Now(), FindingID: 1, ApprovedBy: 3,
		ProtectedRoles: []string{target.User}})
	if !errors.Is(err, ErrBackendEvidenceStale) {
		t.Fatalf("protected role = %v, want no match", err)
	}
	backend.waitUncancelled(t)
}

func TestCancelBackendWithheldByPolicyNeverSignals(t *testing.T) {
	pool, ctx := requireDB(t)
	backend := startSleepingBackend(t, pool, 2)
	exec, _ := cancelExecutor(pool, policy.Decision{Verdict: policy.VerdictBlocked,
		Reason: policy.ReasonOutsideMaintenanceWindow})
	_, err := exec.CancelBackend(ctx, BackendCancel{
		Target: sleeperIdentity(t, pool, backend), ObservedAt: time.Now(),
		FindingID: 1, ApprovedBy: 3})
	if !errors.Is(err, ErrActionWithheld) {
		t.Fatalf("withheld cancel = %v, want ErrActionWithheld", err)
	}
	backend.waitUncancelled(t)
}

// flipGate authorizes the first call and refuses every later one: an
// emergency stop raised between authorization and re-authorization.
type flipGate struct {
	mu    sync.Mutex
	calls int
}

func (g *flipGate) Authorize(context.Context, policy.ActionRequest) policy.Decision {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	if g.calls == 1 {
		return executeVerdict()
	}
	return policy.Decision{Verdict: policy.VerdictBlocked, Reason: policy.ReasonEmergencyStop}
}

// CHECK-17: an emergency stop raised while the action waits stops it at
// re-authorization; one raised at the last moment stops it at the
// mutation block.
func TestCancelBackendEmergencyStopStopsTheSignal(t *testing.T) {
	pool, ctx := requireDB(t)
	backend := startSleepingBackend(t, pool, 3)
	exec := New(pool, advisoryConfig(), time.Time{}, nopLog)
	exec.emergencyStopFn = func(context.Context) bool { return false }
	exec.WithPolicyGate(&flipGate{})
	target := sleeperIdentity(t, pool, backend)
	_, err := exec.CancelBackend(ctx, BackendCancel{Target: target,
		ObservedAt: time.Now(), FindingID: 1, ApprovedBy: 3})
	var withheld *WithheldError
	if !errors.As(err, &withheld) || !withheld.Reauthorized {
		t.Fatalf("stop between authorizations = %v, want re-authorization refusal", err)
	}

	exec, _ = cancelExecutor(pool, executeVerdict())
	exec.emergencyStopFn = func(context.Context) bool { return true }
	_, err = exec.CancelBackend(ctx, BackendCancel{Target: target,
		ObservedAt: time.Now(), FindingID: 1, ApprovedBy: 3})
	if err == nil || !strings.Contains(err.Error(), "emergency stop") {
		t.Fatalf("stop at the signal = %v, want emergency stop refusal", err)
	}
	backend.waitUncancelled(t)
}

func TestCancelBackendSQLRechecksTheWholeIdentity(t *testing.T) {
	for _, predicate := range []string{
		"a.pid = $1", "a.backend_start = $2", "a.query_start = $3",
		"a.datname = $4", "a.datname = pg_catalog.current_database()",
		"a.usename = $5", "sha256(convert_to(a.query, 'UTF8'))", "COALESCE(a.query_id, 0) = $7",
		"a.state = 'active'", "a.backend_type = 'client backend'",
		"a.pid <> pg_catalog.pg_backend_pid()", "pg_sage", "pg_dump",
		"a.usename <> ALL ($8::text[])",
		"a.application_name <> ALL ($9::text[])", "pg_catalog.pg_cancel_backend(a.pid)",
	} {
		if !strings.Contains(cancelBackendIdentitySQL, predicate) {
			t.Errorf("cancel SQL lacks %q", predicate)
		}
	}
	if strings.Contains(cancelBackendIdentitySQL, "pg_terminate_backend") {
		t.Fatal("cancel SQL terminates")
	}
}

func TestPreviewBackendCancelExplainsWithoutRecording(t *testing.T) {
	exec := New(nil, advisoryConfig(), time.Time{}, nopLog)
	withTestStandingGate(exec)
	got := exec.PreviewBackendCancel(context.Background(), false)
	if got.Decision != PolicyDecisionExecute || got.RiskTier != "moderate" {
		t.Fatalf("preview = %+v, want an authorizable moderate cancel", got)
	}
	got = exec.PreviewBackendCancel(context.Background(), true)
	if got.Decision == PolicyDecisionExecute || got.BlockedReason != "replica_mutation" {
		t.Fatalf("replica preview = %+v, want replica_mutation", got)
	}
	observing := New(nil, config.DefaultConfig(), time.Time{}, nopLog)
	withTestStandingGate(observing)
	got = observing.PreviewBackendCancel(context.Background(), false)
	if got.Decision == PolicyDecisionExecute || got.BlockedReason != "observe_only" {
		t.Fatalf("observation preview = %+v, want observe_only", got)
	}
	none := New(nil, advisoryConfig(), time.Time{}, nopLog)
	if got := none.PreviewBackendCancel(context.Background(), false); got.Decision ==
		PolicyDecisionExecute {
		t.Fatalf("preview without a gate = %+v, want fail closed", got)
	}
}

// insertCancelFinding inserts the open finding a Sage SRE approval item
// hangs off: no recommended SQL, so the manual path can never run it.
func insertCancelFinding(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var id int
	err := pool.QueryRow(context.Background(), `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail)
		VALUES ('sre_action', 'critical', 'backend', $1, 'cancel test', '{}')
		RETURNING id`, fmt.Sprintf("sre_proposal:%d", time.Now().UnixNano())).Scan(&id)
	if err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	return id
}
