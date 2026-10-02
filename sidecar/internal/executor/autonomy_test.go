package executor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// Sage SRE M7: custodian actions are remediations of an incident family
// (freeze -> wraparound_runway, WAL bound -> wal_retention), so the
// earned-autonomy ledger governs them; an L2 verdict becomes a one-click
// approval handoff; decisions name the family and class so live outcomes
// can be attributed; a re-authorization under the action's own lease
// says so, so the ledger does not count that lease as a concurrent writer.

func TestCustodianRequestCarriesTheIncidentFamily(t *testing.T) {
	observed := time.Date(2026, 10, 2, 3, 0, 0, 0, time.UTC)
	cases := map[string]string{
		"freeze": "wraparound_runway", "freeze_blocker": "wraparound_runway",
		"autovacuum_tuning": "wraparound_runway", "wal": "wal_retention",
		"schema_guard": "", "fk_index": "",
	}
	for feature, family := range cases {
		req := custodianRequest(CustodianProposal{Feature: feature,
			SQL: `VACUUM (FREEZE) "public"."orders"`, TargetObjects: []string{"public.orders"},
			ObservedAt: observed})
		if req.IncidentFamily != family || !req.EvidenceObservedAt.Equal(observed) {
			t.Errorf("%s: family %q observed %v; want %q %v", feature, req.IncidentFamily,
				req.EvidenceObservedAt, family, observed)
		}
	}
}

type countingLimiter struct {
	mu    sync.Mutex
	limit policy.AutonomyLimit
	calls int
}

func (l *countingLimiter) Limit(context.Context, policy.ActionRequest) (
	policy.AutonomyLimit, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	return l.limit, nil
}

func autonomousConfig() *config.Config {
	return &config.Config{Trust: config.TrustConfig{Level: "autonomous", Tier3Safe: true,
		Tier3Moderate: true}}
}

func freezeProposal() CustodianProposal {
	return CustodianProposal{Feature: "freeze", SQL: `VACUUM (FREEZE) "public"."orders"`,
		TargetObjects: []string{"public.orders"}, ObservedAt: time.Now()}
}

func TestWithAutonomyRestrictsTheStandingGate(t *testing.T) {
	limiter := &countingLimiter{limit: policy.AutonomyLimit{Level: 1, Granted: 1}}
	exec := New(nil, autonomousConfig(), time.Now().Add(-60*24*time.Hour), noopExecLog)
	exec.emergencyStopFn = func(context.Context) bool { return false }
	exec.SetExecutionMode("auto")
	exec.WithAutonomy(limiter)
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	exec.EnableStandingPolicyDocument(doc, nil)
	got := exec.EvaluateCustodianProposal(context.Background(), freezeProposal())
	if got.Decision != PolicyDecisionObserveOnly ||
		got.BlockedReason != string(policy.ReasonAutonomyLevel) || limiter.calls != 1 {
		t.Fatalf("decision = %+v, limiter calls = %d", got, limiter.calls)
	}
	limiter.limit = policy.AutonomyLimit{Level: 3, Granted: 3}
	got = exec.EvaluateCustodianProposal(context.Background(), freezeProposal())
	if got.Decision != PolicyDecisionExecute ||
		got.BlockedReason != string(policy.ReasonAutonomyL3) {
		t.Fatalf("L3 decision = %+v", got)
	}
}

func TestWithoutAutonomyTheGateIsUnchanged(t *testing.T) {
	exec := New(nil, autonomousConfig(), time.Now().Add(-60*24*time.Hour), noopExecLog)
	exec.emergencyStopFn = func(context.Context) bool { return false }
	exec.SetExecutionMode("auto")
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	exec.EnableStandingPolicyDocument(doc, nil)
	got := exec.EvaluateCustodianProposal(context.Background(), freezeProposal())
	if got.Decision != PolicyDecisionExecute {
		t.Fatalf("decision without the ledger = %+v", got)
	}
}

func TestDecisionEvidenceNamesFamilyAndClass(t *testing.T) {
	req := custodianRequest(freezeProposal())
	in := ledgerInput(nil, 1, req, policy.Decision{Verdict: policy.VerdictExecute,
		Reason: policy.ReasonAutonomyL3, RiskTier: policy.RiskSafe})
	if in.Evidence["incident_family"] != "wraparound_runway" ||
		in.Evidence["autonomy_class"] != "freeze" {
		t.Fatalf("decision evidence = %#v", in.Evidence)
	}
	plain := ledgerInput(nil, 1, policy.ActionRequest{Feature: "index"},
		policy.Decision{Verdict: policy.VerdictExecute})
	if _, ok := plain.Evidence["incident_family"]; ok {
		t.Fatalf("a request without a family names one: %#v", plain.Evidence)
	}
}

// handoffExecutor takes the package database once per test: requireDB holds
// the cross-package test lock until the test ends, so a second call in
// the same test would wait for itself.
func handoffExecutor(t *testing.T, level int) (*Executor, *pgxpool.Pool) {
	t.Helper()
	pool, _ := requireDB(t)
	limiter := &countingLimiter{limit: policy.AutonomyLimit{Level: level, Granted: level}}
	exec := New(pool, autonomousConfig(), time.Now().Add(-60*24*time.Hour), noopExecLog)
	exec.WithActionStore(store.NewActionStore(pool), "auto")
	exec.WithAutonomy(limiter)
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	exec.EnableStandingPolicyDocument(doc, nil)
	exec.WithEmergencyStopCheck(func(context.Context) bool { return false })
	return exec, pool
}

func pendingHandoffs(t *testing.T, pool *pgxpool.Pool, key string) int {
	t.Helper()
	ctx := context.Background()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.action_queue
		WHERE identity_key = $1 AND status = 'pending'`, key).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func clearHandoffs(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	ctx := context.Background()
	clean := func() {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.action_queue WHERE identity_key = $1", key)
	}
	clean()
	t.Cleanup(clean)
}

const freezeHandoffKey = "autonomy:wraparound_runway:freeze:public.orders"

func TestL2VerdictQueuesOneApprovalHandoff(t *testing.T) {
	exec, pool := handoffExecutor(t, 2)
	clearHandoffs(t, pool, freezeHandoffKey)
	for i := 0; i < 2; i++ {
		if err := exec.SubmitCustodianProposal(context.Background(), freezeProposal()); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	if n := pendingHandoffs(t, pool, freezeHandoffKey); n != 1 {
		t.Fatalf("pending handoffs = %d, want exactly 1", n)
	}
	ctx := context.Background()
	var actionType, decision, sql string
	if err := pool.QueryRow(ctx, `SELECT action_type, policy_decision, proposed_sql
		FROM sage.action_queue WHERE identity_key = $1`, freezeHandoffKey).
		Scan(&actionType, &decision, &sql); err != nil {
		t.Fatal(err)
	}
	if actionType != "vacuum_table" || decision != PolicyDecisionQueueApproval ||
		sql != freezeProposal().SQL {
		t.Fatalf("handoff = %s %s %q", actionType, decision, sql)
	}
}

func TestL1VerdictIsAManualScriptNotAHandoff(t *testing.T) {
	exec, pool := handoffExecutor(t, 1)
	clearHandoffs(t, pool, freezeHandoffKey)
	err := exec.SubmitCustodianProposal(context.Background(), freezeProposal())
	if !errors.Is(err, ErrCustodianProposalWithheld) {
		t.Fatalf("submit at L1: %v", err)
	}
	if n := pendingHandoffs(t, pool, freezeHandoffKey); n != 0 {
		t.Fatalf("pending handoffs at L1 = %d", n)
	}
}

func TestRejectedHandoffIsNotReproposedAtOnce(t *testing.T) {
	exec, pool := handoffExecutor(t, 2)
	clearHandoffs(t, pool, freezeHandoffKey)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO sage.action_queue (proposed_sql, action_risk,
		status, identity_key, decided_at) VALUES ($1, 'safe', 'rejected', $2, now())`,
		freezeProposal().SQL, freezeHandoffKey); err != nil {
		t.Fatal(err)
	}
	if err := exec.SubmitCustodianProposal(context.Background(), freezeProposal()); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if n := pendingHandoffs(t, pool, freezeHandoffKey); n != 0 {
		t.Fatalf("re-proposed a handoff the operator just rejected: %d", n)
	}
}

func TestHandoffWithoutAnActionStoreStaysWithheld(t *testing.T) {
	pool, _ := requireDB(t)
	exec := New(pool, autonomousConfig(), time.Now().Add(-60*24*time.Hour), noopExecLog)
	exec.WithAutonomy(&countingLimiter{limit: policy.AutonomyLimit{Level: 2, Granted: 2}})
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	exec.EnableStandingPolicyDocument(doc, nil)
	exec.WithEmergencyStopCheck(func(context.Context) bool { return false })
	exec.SetExecutionMode("auto")
	err := exec.SubmitCustodianProposal(context.Background(), freezeProposal())
	if !errors.Is(err, ErrCustodianProposalWithheld) {
		t.Fatalf("handoff without a queue: %v", err)
	}
}

type leaseCaptureGate struct {
	mu       sync.Mutex
	decision int64
	seen     []bool
}

func (g *leaseCaptureGate) Authorize(_ context.Context, req policy.ActionRequest) policy.Decision {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.seen = append(g.seen, req.LeaseHeld)
	return policy.Decision{Verdict: policy.VerdictExecute, RiskTier: policy.RiskModerate,
		Reason: policy.ReasonAuthorized, DecisionID: g.decision}
}

func TestReauthorizationUnderTheLeaseSaysSo(t *testing.T) {
	pool, ctx := requireDB(t)
	var decision int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.decision (feature, intent, verdict,
		risk_tier, reason, evidence_id) VALUES ('index', 'index', 'execute', 'moderate',
		'authorized', md5(random()::text)) RETURNING id`).Scan(&decision); err != nil {
		t.Fatal(err)
	}
	gate := &leaseCaptureGate{decision: decision}
	exec := New(pool, autonomousConfig(), time.Time{}, noopExecLog)
	exec.WithPolicyGate(gate)
	finding := analyzer.Finding{ObjectIdentifier: "public.m7_lease_probe",
		RecommendedSQL: "CREATE INDEX CONCURRENTLY m7_idx ON public.m7_lease_probe (id)"}
	_, err := exec.Apply(ctx, ActionIntent{
		Request: policy.ActionRequest{IncidentFamily: "plan_regression"},
		Lease:   &finding, WaitForSlot: true,
		Execute: func(context.Context, ActionPolicyDecision) (int64, error) { return 0, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(gate.seen) != 2 || gate.seen[0] || !gate.seen[1] {
		t.Fatalf("LeaseHeld per authorization = %v, want [false true]", gate.seen)
	}
}
