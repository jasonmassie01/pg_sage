package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/policy"
)

// executor-10: the full-binary e2e test (e2e/pipeline_binary_test.go) saw
// every Layer A change queued. These pin, through evaluateFindingPolicy,
// the verdict each of its seeded findings gets from the standing gate:
// granted L3 with the window "always" executes, a build without the
// parse-tree layer queues with sql_validation_degraded, and a demoted
// class withholds.
// No concurrent access tests: evaluateFindingPolicy is stateless per call
// and withDegradedASTValidation swaps a package variable (not parallel-safe).
// No window or ramp boundary tests: neither caused the e2e withholds; the
// window boundaries are pinned in internal/policy/gate_window tests.

// pipelineSeedFindings are the Layer A mutating findings as the analyzer
// emits them.
func pipelineSeedFindings() []analyzer.Finding {
	return []analyzer.Finding{
		{Category: "duplicate_index", ObjectType: "index",
			ObjectIdentifier: "public.pipe_dup_b",
			RecommendedSQL:   "DROP INDEX CONCURRENTLY public.pipe_dup_b;"},
		{Category: "table_bloat", ObjectType: "table", ObjectIdentifier: "public.pipe_bloat",
			RecommendedSQL: "VACUUM public.pipe_bloat;"},
		{Category: "stale_statistics", ObjectType: "table",
			ObjectIdentifier: "public.pipe_stale", RecommendedSQL: "ANALYZE public.pipe_stale;"},
		{Category: "autovacuum_tuning", ObjectType: "table", ObjectIdentifier: "public.pipe_av",
			RecommendedSQL: "ALTER TABLE public.pipe_av " +
				"SET (autovacuum_vacuum_scale_factor = 0.05);"},
		{Category: "invalid_index", ObjectType: "index",
			ObjectIdentifier: "public.pipe_invalid_idx",
			RecommendedSQL:   "DROP INDEX CONCURRENTLY public.pipe_invalid_idx;"},
	}
}

// levelExecutor is the e2e binary's gate with every class at level.
func levelExecutor(level int, tier3Safe bool) *Executor {
	cfg := autonomousConfig()
	cfg.Trust.MaintenanceWindow = "always"
	cfg.Trust.Tier3Safe = tier3Safe
	exec := New(nil, cfg, time.Now().Add(-60*24*time.Hour), noopExecLog)
	exec.emergencyStopFn = func(context.Context) bool { return false }
	exec.SetExecutionMode("auto")
	exec.WithAutonomy(scopedLimiter{limit: policy.AutonomyLimit{Level: level, Granted: level}})
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	exec.EnableStandingPolicyDocument(doc, nil)
	return exec
}

func TestPipelineSeedsExecuteAtGrantedL3(t *testing.T) {
	exec := levelExecutor(3, true)
	for _, f := range pipelineSeedFindings() {
		got := exec.evaluateFindingPolicy(context.Background(), f, false)
		if got.Decision != PolicyDecisionExecute ||
			got.BlockedReason != string(policy.ReasonAutonomyL3) {
			t.Errorf("%s at granted L3: %s/%q (%s), want execute %s", f.Category,
				got.Decision, got.BlockedReason, got.Detail, policy.ReasonAutonomyL3)
		}
		if got.RequiresApproval {
			t.Errorf("%s at granted L3 requires approval: %+v", f.Category, got)
		}
	}
}

// The e2e triage verdict: on a cgo-less build every seed is queued even at
// granted L3. The ledger records its handoff reason over the base queue
// (pinned by policy.TestAutonomyNeverWidensTheOperatorBound); the cause,
// sql_validation_degraded, is named only in the detail.
func TestPipelineSeedsQueueWithoutParseTreeValidation(t *testing.T) {
	withDegradedASTValidation(t)
	exec := levelExecutor(3, true)
	for _, f := range pipelineSeedFindings() {
		got := exec.evaluateFindingPolicy(context.Background(), f, false)
		if got.Decision != PolicyDecisionQueueApproval || !got.RequiresApproval ||
			got.BlockedReason != string(policy.ReasonAutonomyHandoff) {
			t.Errorf("%s on a cgo-less build: %s/%q, want queue_approval %s",
				f.Category, got.Decision, got.BlockedReason, policy.ReasonAutonomyHandoff)
		}
		want := "autonomy L3 (granted L3, class ); restricted " +
			string(policy.ReasonSQLValidationDegraded) + " (built without cgo"
		if !strings.HasPrefix(got.Detail, want) {
			t.Errorf("%s detail %q, want it to start %q", f.Category, got.Detail, want)
		}
	}
}

// State transition: the grandfathered L3 executes; a demotion of the class
// (a regressed verdict, a rollback or a rejection) withholds the next run.
func TestPipelineSeedsWithholdAfterDemotion(t *testing.T) {
	cases := []struct {
		level  int
		want   string
		reason policy.Reason
	}{
		{2, PolicyDecisionQueueApproval, policy.ReasonAutonomyHandoff},
		{1, PolicyDecisionObserveOnly, policy.ReasonAutonomyLevel},
		{0, PolicyDecisionObserveOnly, policy.ReasonAutonomyLevel},
	}
	for _, c := range cases {
		exec := levelExecutor(c.level, true)
		for _, f := range pipelineSeedFindings() {
			got := exec.evaluateFindingPolicy(context.Background(), f, false)
			if got.Decision != c.want || got.BlockedReason != string(c.reason) {
				t.Errorf("%s demoted to L%d: %s/%q, want %s/%s", f.Category, c.level,
					got.Decision, got.BlockedReason, c.want, c.reason)
			}
		}
	}
}

// Invalid input: autonomous trust with tier3_safe off withholds the SAFE
// hygiene seeds even at granted L3 (the operator bound wins).
func TestPipelineSafeSeedsBlockedWithTierSafeOff(t *testing.T) {
	exec := levelExecutor(3, false)
	for _, f := range pipelineSeedFindings()[1:3] {
		got := exec.evaluateFindingPolicy(context.Background(), f, false)
		if got.Decision != PolicyDecisionBlocked ||
			got.BlockedReason != string(policy.ReasonTrustRampNotSatisfied) {
			t.Errorf("%s with tier3_safe off: %s/%q, want blocked %s", f.Category,
				got.Decision, got.BlockedReason, policy.ReasonTrustRampNotSatisfied)
		}
	}
}

// Nil/empty: without a standing gate nothing executes (fail closed).
func TestPipelineSeedsWithoutStandingGateNeverExecute(t *testing.T) {
	exec := New(nil, autonomousConfig(), time.Now().Add(-60*24*time.Hour), noopExecLog)
	for _, f := range pipelineSeedFindings() {
		got := exec.evaluateFindingPolicy(context.Background(), f, false)
		if got.Decision == PolicyDecisionExecute || got.BlockedReason == "" {
			t.Errorf("%s without a standing gate: %s/%q, want a withheld verdict "+
				"with a reason", f.Category, got.Decision, got.BlockedReason)
		}
	}
}
