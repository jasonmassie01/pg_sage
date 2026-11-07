package executor

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/policy"
)

// facts-01 review: keeping a non-relation target away from the facts must
// not take it away from the rest of the gate. Earned L3 acts on exactly
// one object, so a configuration finding with no target could never earn
// autonomous execution.

// No concurrent access tests here: findingRequest builds a fresh request.

// scopedLimiter grants a fixed level and governs every request, like the
// earned ledger governs self-initiated classes.
type scopedLimiter struct{ limit policy.AutonomyLimit }

func (l scopedLimiter) Limit(context.Context, policy.ActionRequest) (
	policy.AutonomyLimit, error) {
	return l.limit, nil
}

func (scopedLimiter) Governs(policy.ActionRequest) bool { return true }

func l3Executor(binder policy.FactBinder) *Executor {
	cfg := autonomousConfig()
	cfg.Trust.MaintenanceWindow = "always"
	exec := New(nil, cfg, time.Now().Add(-60*24*time.Hour), noopExecLog)
	exec.emergencyStopFn = func(context.Context) bool { return false }
	exec.SetExecutionMode("auto")
	exec.WithAutonomy(scopedLimiter{limit: policy.AutonomyLimit{Level: 3, Granted: 3}})
	if binder != nil {
		exec.WithFactBinder(binder)
	}
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	exec.EnableStandingPolicyDocument(doc, nil)
	return exec
}

func TestConfigFindingKeepsItsOneTargetForEarnedL3(t *testing.T) {
	finding := configFinding("ALTER SYSTEM SET work_mem = '64MB'")
	for _, binder := range []policy.FactBinder{nil, facts.NewBinder(fixtureFact(), nil, nil)} {
		got := l3Executor(binder).evaluateFindingPolicy(context.Background(), finding, false)
		if got.Decision != PolicyDecisionExecute ||
			got.BlockedReason != string(policy.ReasonAutonomyL3) {
			t.Fatalf("binder %v: config finding at granted L3: %+v, want execute %s",
				binder != nil, got, policy.ReasonAutonomyL3)
		}
	}
}

func TestFindingRequestTargetsAndFactTargets(t *testing.T) {
	cases := []struct {
		objectType, id string
		targets        []string
		noRelation     bool
	}{
		{"configuration", "instance", []string{"instance"}, true},
		{"configuration", "instance:work_mem", []string{"instance:work_mem"}, true},
		{"setting", "work_mem", []string{"work_mem"}, true},
		{"database", "lifeos", []string{"lifeos"}, true},
		{"Instance", "instance", []string{"instance"}, true},
		{"query", "queryid:42", []string{"queryid:42"}, true},
		{"table", "public.orders", []string{"public.orders"}, false},
		{"advisor", "llm", []string{"llm"}, false},
		{"configuration", " ", nil, true},
	}
	for _, c := range cases {
		req := findingRequest(analyzer.Finding{ObjectType: c.objectType,
			ObjectIdentifier: c.id, RecommendedSQL: "ANALYZE public.orders"}, false)
		if len(req.TargetObjs) != len(c.targets) ||
			(len(c.targets) == 1 && req.TargetObjs[0] != c.targets[0]) {
			t.Errorf("%s %q: targets %v, want %v", c.objectType, c.id, req.TargetObjs,
				c.targets)
		}
		if req.TargetsNameNoRelation != c.noRelation {
			t.Errorf("%s %q: TargetsNameNoRelation %v, want %v", c.objectType, c.id,
				req.TargetsNameNoRelation, c.noRelation)
		}
	}
}
