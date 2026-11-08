package api

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/policy"
)

// enrichCaseActionPolicies enriches one case through the batched path, as
// the cases handler does for a database that has a single case.
func enrichCaseActionPolicies(c *cases.Case, mgr *fleet.DatabaseManager, name string) {
	var inst *fleet.DatabaseInstance
	if mgr != nil {
		inst = mgr.GetInstance(name)
	}
	all := []cases.Case{*c}
	enrichCasesActionPolicies(context.Background(), all, inst)
	*c = all[0]
}

// countingBatchGate wraps a standing gate and counts the batch explains
// the cases path sends, with the action types of each.
type countingBatchGate struct {
	inner policy.Gate
	mu    sync.Mutex
	types [][]string
}

func (g *countingBatchGate) Authorize(ctx context.Context, r policy.ActionRequest) policy.Decision {
	return g.inner.Authorize(ctx, r)
}

func (g *countingBatchGate) ExplainBatch(
	ctx context.Context, requests []policy.ActionRequest,
) []policy.Decision {
	sent := make([]string, len(requests))
	for i, r := range requests {
		sent[i] = r.Contract.ActionType
	}
	g.mu.Lock()
	g.types = append(g.types, sent)
	g.mu.Unlock()
	return g.inner.(policy.BatchExplainer).ExplainBatch(ctx, requests)
}

func (g *countingBatchGate) calls() [][]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([][]string(nil), g.types...)
}

func countingCaseInstance(
	cfg *config.Config, status *fleet.InstanceStatus,
) (*fleet.DatabaseInstance, *countingBatchGate) {
	exec := caseGateExecutor(cfg)
	gate := &countingBatchGate{inner: exec.StandingPolicyGate()}
	exec.WithPolicyGate(gate)
	return &fleet.DatabaseInstance{
		Name: "prod", Config: config.DatabaseConfig{Name: "prod", ExecutionMode: "auto"},
		Executor: exec, Status: status,
	}, gate
}

var batchObservedAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// batchFixtureCases is n finding cases over three action types, every
// fifth with an extra unknown candidate and every seventh with no SQL.
func batchFixtureCases(n int) []cases.Case {
	sqls := []string{
		"ANALYZE public.orders",
		"CREATE INDEX CONCURRENTLY idx_orders_c ON public.orders (c)",
		"VACUUM public.orders",
	}
	out := make([]cases.Case, 0, n)
	for i := 0; i < n; i++ {
		f := cases.SourceFinding{
			ID: fmt.Sprint(i + 1), DatabaseName: "prod", Category: "stale_statistics",
			Severity: cases.SeverityWarning, ObjectType: "table",
			ObjectIdentifier: fmt.Sprintf("public.t%d", i), Title: fmt.Sprintf("finding %d", i),
			RecommendedSQL: sqls[i%len(sqls)], ObservedAt: batchObservedAt,
		}
		if i%7 == 6 {
			f.RecommendedSQL = ""
		}
		c := cases.ProjectFinding(f)
		if i%5 == 4 {
			c.ActionCandidates = append(c.ActionCandidates,
				cases.ActionCandidate{ActionType: "mystery_family"})
		}
		out = append(out, c)
	}
	return out
}

// cloneCases copies the cases and their candidate slices, so enriching the
// copy leaves the original untouched.
func cloneCases(in []cases.Case) []cases.Case {
	out := make([]cases.Case, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].ActionCandidates = append([]cases.ActionCandidate(nil), in[i].ActionCandidates...)
	}
	return out
}

// perCaseReference is the per-case enrichment the handler ran before
// batching: one explain per case through the instance executor.
func perCaseReference(c *cases.Case, inst *fleet.DatabaseInstance) {
	caps := inst.SnapshotStatus().Capabilities
	defer cases.ApplyProviderApplicability(c, caps.Provider)
	var contracts []executor.ActionContract
	var slots []int
	for i := range c.ActionCandidates {
		contract, ok := executor.ContractForActionType(c.ActionCandidates[i].ActionType)
		if !ok {
			c.ActionCandidates[i].BlockedReason = "unknown action type"
			continue
		}
		contracts = append(contracts, contract)
		slots = append(slots, i)
	}
	if len(contracts) == 0 {
		return
	}
	decisions := inst.Executor.ExplainFamilies(context.Background(), contracts, caps.IsReplica)
	for k, decision := range decisions {
		applyCandidatePolicy(&c.ActionCandidates[slots[k]], decision)
	}
}

func referenceCases(
	t *testing.T, cfg *config.Config, status *fleet.InstanceStatus, in []cases.Case,
) []cases.Case {
	t.Helper()
	ref, _ := countingCaseInstance(cfg, status)
	want := cloneCases(in)
	for i := range want {
		perCaseReference(&want[i], ref)
	}
	return want
}

func TestEnrichCasesActionPoliciesExplainsOncePerDatabase(t *testing.T) {
	cfg := caseAutonomousConfig()
	inst, gate := countingCaseInstance(cfg, nil)
	got := batchFixtureCases(300)
	want := referenceCases(t, cfg, nil, got)

	enrichCasesActionPolicies(context.Background(), got, inst)

	calls := gate.calls()
	if len(calls) != 1 {
		t.Fatalf("explain calls = %d, want 1 for one database", len(calls))
	}
	wantTypes := []string{"analyze_table", "create_index_concurrently", "vacuum_table"}
	if !reflect.DeepEqual(calls[0], wantTypes) {
		t.Fatalf("explained types = %v, want each distinct type once: %v", calls[0], wantTypes)
	}
	for i := range got {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Fatalf("case %d differs from the per-case path:\n got %#v\nwant %#v",
				i, got[i].ActionCandidates, want[i].ActionCandidates)
		}
	}
	first := got[0].ActionCandidates[0]
	if d := first.PolicyDecision; d == nil || d.Decision != executor.PolicyDecisionExecute {
		t.Fatalf("analyze verdict = %#v, want execute under autonomous trust", first.PolicyDecision)
	}
}

func TestEnrichCasesActionPoliciesNoCandidatesNoCall(t *testing.T) {
	inst, gate := countingCaseInstance(caseAutonomousConfig(), nil)
	enrichCasesActionPolicies(context.Background(), nil, inst)
	enrichCasesActionPolicies(context.Background(), []cases.Case{}, inst)
	empty := []cases.Case{{DatabaseName: "prod"}, {DatabaseName: "prod", Title: "no action"}}
	enrichCasesActionPolicies(context.Background(), empty, inst)

	if calls := gate.calls(); len(calls) != 0 {
		t.Fatalf("explain calls = %v, want none without candidates", calls)
	}
	if empty[1].Title != "no action" || len(empty[1].ActionCandidates) != 0 {
		t.Fatalf("case without candidates changed: %#v", empty[1])
	}
}

func TestEnrichCasesActionPoliciesUnknownTypeNeverSent(t *testing.T) {
	inst, gate := countingCaseInstance(caseAutonomousConfig(), nil)
	only := []cases.Case{{DatabaseName: "prod", ActionCandidates: []cases.ActionCandidate{
		{ActionType: "mystery_family"}, {ActionType: ""},
	}}}
	enrichCasesActionPolicies(context.Background(), only, inst)
	if calls := gate.calls(); len(calls) != 0 {
		t.Fatalf("explain calls = %v, want none for unknown types only", calls)
	}
	for i, candidate := range only[0].ActionCandidates {
		if candidate.BlockedReason != "unknown action type" || candidate.PolicyDecision != nil {
			t.Fatalf("candidate %d = %#v, want blocked unknown action type", i, candidate)
		}
	}

	mixed := []cases.Case{{DatabaseName: "prod", ActionCandidates: []cases.ActionCandidate{
		{ActionType: "mystery_family"}, {ActionType: "analyze_table", RiskTier: "safe"},
	}}}
	enrichCasesActionPolicies(context.Background(), mixed, inst)
	calls := gate.calls()
	if len(calls) != 1 || !reflect.DeepEqual(calls[0], []string{"analyze_table"}) {
		t.Fatalf("explained = %v, want only analyze_table", calls)
	}
	if mixed[0].ActionCandidates[0].BlockedReason != "unknown action type" {
		t.Fatalf("unknown candidate = %#v", mixed[0].ActionCandidates[0])
	}
	if d := mixed[0].ActionCandidates[1].PolicyDecision; d == nil ||
		d.Decision != executor.PolicyDecisionExecute {
		t.Fatalf("known candidate decision = %#v, want execute", d)
	}
}

// A cancelled request fails closed as policy_unavailable without reaching
// the gate (the standing gate's readers would otherwise run).
func TestEnrichCasesActionPoliciesCancelledContextFailsClosed(t *testing.T) {
	inst, gate := countingCaseInstance(caseAutonomousConfig(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := batchFixtureCases(30)

	enrichCasesActionPolicies(ctx, got, inst)

	if calls := gate.calls(); len(calls) != 0 {
		t.Fatalf("explain calls after cancel = %v, want none", calls)
	}
	known := 0
	for _, c := range got {
		for _, candidate := range c.ActionCandidates {
			if candidate.ActionType == "mystery_family" {
				continue
			}
			known++
			d := candidate.PolicyDecision
			if d == nil || d.Decision != executor.PolicyDecisionBlocked ||
				d.BlockedReason != string(policy.ReasonPolicyUnavailable) ||
				candidate.BlockedReason != string(policy.ReasonPolicyUnavailable) {
				t.Fatalf("candidate %s = %#v / %#v, want blocked policy_unavailable",
					candidate.ActionType, candidate, d)
			}
		}
	}
	if known == 0 {
		t.Fatal("fixture has no known candidates")
	}
}

func TestEnrichCasesActionPoliciesFailsClosedForNilAndStopped(t *testing.T) {
	got := batchFixtureCases(6)
	enrichCasesActionPolicies(context.Background(), got, nil)
	assertAllKnownBlocked(t, got, "standing policy unavailable")

	inst, gate := countingCaseInstance(caseAutonomousConfig(), nil)
	inst.Stopped = true
	stopped := batchFixtureCases(6)
	enrichCasesActionPolicies(context.Background(), stopped, inst)
	assertAllKnownBlocked(t, stopped, "emergency stop is active")
	if calls := gate.calls(); len(calls) != 0 {
		t.Fatalf("stopped instance reached the gate: %v", calls)
	}
}

func assertAllKnownBlocked(t *testing.T, all []cases.Case, reason string) {
	t.Helper()
	for i, c := range all {
		for _, candidate := range c.ActionCandidates {
			want := reason
			if candidate.ActionType == "mystery_family" {
				want = "unknown action type"
			}
			if candidate.BlockedReason != want {
				t.Fatalf("case %d %s BlockedReason = %q, want %q",
					i, candidate.ActionType, candidate.BlockedReason, want)
			}
		}
	}
}

// Provider applicability stays per case: a Lakebase ALTER SYSTEM candidate
// is withheld while the other cases keep their gate verdict.
func TestEnrichCasesActionPoliciesKeepsProviderApplicabilityPerCase(t *testing.T) {
	cfg := caseAutonomousConfig()
	status := &fleet.InstanceStatus{Platform: "lakebase",
		Capabilities: fleet.ProviderCapabilities{Provider: "lakebase"}}
	inst, gate := countingCaseInstance(cfg, status)
	got := batchFixtureCases(4)
	got = append(got, cases.ProjectFinding(cases.SourceFinding{
		ID: "99", DatabaseName: "prod", Category: "config", Severity: cases.SeverityWarning,
		Title: "work_mem", RecommendedSQL: "ALTER SYSTEM SET work_mem = '64MB'",
		ObservedAt: batchObservedAt,
	}))
	want := referenceCases(t, cfg, status, got[:4])
	wantLast := cloneCases(got[4:])
	ref, _ := countingCaseInstance(cfg, status)
	perCaseReference(&wantLast[0], ref)

	enrichCasesActionPolicies(context.Background(), got, inst)

	if calls := gate.calls(); len(calls) != 1 {
		t.Fatalf("explain calls = %d, want 1", len(calls))
	}
	if !reflect.DeepEqual(got[:4], want) || !reflect.DeepEqual(got[4], wantLast[0]) {
		t.Fatalf("batched cases differ from the per-case path")
	}
	last := got[4].ActionCandidates[0]
	if d := last.PolicyDecision; d == nil || d.BlockedReason != "provider_managed_setting" {
		t.Fatalf("lakebase ALTER SYSTEM candidate = %#v, want provider_managed_setting", last)
	}
	if d := got[0].ActionCandidates[0].PolicyDecision; d == nil ||
		d.BlockedReason == "provider_managed_setting" {
		t.Fatalf("analyze candidate on lakebase = %#v, want the gate verdict", d)
	}
}

// Candidates that share a verdict must not share its guardrail slice: a
// later edit of one case must not leak into another.
func TestEnrichCasesActionPoliciesDoesNotAliasGuardrails(t *testing.T) {
	inst, _ := countingCaseInstance(caseAutonomousConfig(), nil)
	got := batchFixtureCases(4) // cases 0 and 3 are both analyze_table
	enrichCasesActionPolicies(context.Background(), got, inst)
	a, b := &got[0].ActionCandidates[0], &got[3].ActionCandidates[0]
	if len(a.Guardrails) == 0 || len(b.Guardrails) == 0 {
		t.Fatalf("expected guardrails on both analyze candidates")
	}
	original := b.Guardrails[0]
	a.Guardrails[0] = "edited"
	a.PolicyDecision.Guardrails[0] = "edited"
	if b.Guardrails[0] != original || b.PolicyDecision.Guardrails[0] != original {
		t.Fatalf("guardrails aliased across cases: %v / %v", b.Guardrails,
			b.PolicyDecision.Guardrails)
	}
}

// Each call takes a fresh snapshot: disabling the executor between two
// reads changes the next verdict (no cross-request caching).
func TestEnrichCasesActionPoliciesFollowsExecutorState(t *testing.T) {
	inst, gate := countingCaseInstance(caseAutonomousConfig(), nil)
	before := batchFixtureCases(3)
	enrichCasesActionPolicies(context.Background(), before, inst)
	inst.Executor.SetExecutorEnabled(false)
	after := batchFixtureCases(3)
	enrichCasesActionPolicies(context.Background(), after, inst)

	if calls := gate.calls(); len(calls) != 2 {
		t.Fatalf("explain calls = %d, want one per read", len(calls))
	}
	if got := before[0].ActionCandidates[0].PolicyDecision.Decision; got !=
		executor.PolicyDecisionExecute {
		t.Fatalf("before disable = %q, want execute", got)
	}
	if got := after[0].ActionCandidates[0].BlockedReason; got != "executor_disabled" {
		t.Fatalf("after disable BlockedReason = %q, want executor_disabled", got)
	}
}

// Two requests enriching their own cases at once each get one explain and
// the per-case verdicts (run under -race where cgo is available).
func TestEnrichCasesActionPoliciesConcurrentRequests(t *testing.T) {
	cfg := caseAutonomousConfig()
	inst, gate := countingCaseInstance(cfg, nil)
	want := referenceCases(t, cfg, nil, batchFixtureCases(100))
	results := make([][]cases.Case, 2)
	var wg sync.WaitGroup
	for i := range results {
		results[i] = batchFixtureCases(100)
		wg.Add(1)
		go func(all []cases.Case) {
			defer wg.Done()
			enrichCasesActionPolicies(context.Background(), all, inst)
		}(results[i])
	}
	wg.Wait()
	if calls := gate.calls(); len(calls) != 2 {
		t.Fatalf("explain calls = %d, want 2 (one per request)", len(calls))
	}
	for r, got := range results {
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("request %d verdicts differ from the per-case path", r)
		}
	}
}
