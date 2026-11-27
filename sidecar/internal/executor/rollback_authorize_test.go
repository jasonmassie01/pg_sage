package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// policy-05 CHECK-04: the second, unrecorded rollback rule set is gone.
// No production file under internal/ names manualRollback(Authorizer).
func TestUnrecordedRollbackAuthorizerHasNoProductionReference(t *testing.T) {
	var refs []string
	err := filepath.Walk("..", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(raw), "manualRollback"+"Authorizer") {
			refs = append(refs, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("the unrecorded rollback authorizer is still referenced by %v", refs)
	}
}

// The decision of a revert carries evidence.rollback (CHECK-01) and names a
// standing policy it could not read; request evidence cannot forge either.
func TestStampRollbackEvidence(t *testing.T) {
	forged := map[string]any{"rollback": true, "policy_read_error": "forged"}
	stampRollbackEvidence(forged, policy.ActionRequest{}, policy.Decision{})
	if _, ok := forged["rollback"]; ok {
		t.Fatalf("a forward request kept a forged rollback flag: %v", forged)
	}
	if _, ok := forged["policy_read_error"]; ok {
		t.Fatalf("a forward request kept a forged read error: %v", forged)
	}
	evidence := map[string]any{}
	stampRollbackEvidence(evidence, policy.ActionRequest{Rollback: true},
		policy.Decision{PolicyReadError: "connection refused"})
	if evidence["rollback"] != true || evidence["policy_read_error"] != "connection refused" {
		t.Fatalf("rollback evidence = %v", evidence)
	}
	clean := map[string]any{}
	stampRollbackEvidence(clean, policy.ActionRequest{Rollback: true}, policy.Decision{})
	if _, ok := clean["policy_read_error"]; ok || clean["rollback"] != true {
		t.Fatalf("readable policy evidence = %v", clean)
	}
}

// Boundary: the lock ceiling caps only an in-transaction revert; a
// CONCURRENTLY one (including a rewritten index rebuild) keeps the safety
// timeout, and no ceiling keeps the safety timeout.
func TestRollbackLockTimeoutMS(t *testing.T) {
	cases := []struct {
		sql     string
		ceiling int64
		want    int
	}{
		{"ALTER TABLE public.t RESET (fillfactor)", 700, 700},
		{"ALTER TABLE public.t RESET (fillfactor)", 0, 1500},
		{"ALTER TABLE public.t RESET (fillfactor)", 5000, 1500},
		{"DROP INDEX CONCURRENTLY IF EXISTS public.i", 700, 1500},
		{"CREATE INDEX i ON public.t (a)", 700, 1500},
	}
	for _, c := range cases {
		if got := rollbackLockTimeoutMS(c.sql, 1500, c.ceiling); got != c.want {
			t.Fatalf("rollbackLockTimeoutMS(%q, 1500, %d) = %d, want %d", c.sql,
				c.ceiling, got, c.want)
		}
	}
}

// Nil/empty: a revert with no standing gate fails closed, and an
// unclassified rollback reads apart from a policy refusal.
func TestRollbackVerdictFor(t *testing.T) {
	e := New(nil, wave1PolicyConfig("advisory"), time.Time{}, nopLog)
	if d := e.authorizeRollback(context.Background(), 1, probeRollback, true); d.Decision !=
		PolicyDecisionBlocked || d.BlockedReason != reasonNoStandingPolicy {
		t.Fatalf("no gate: %+v", d)
	}
	if v := rollbackVerdictFor(ActionPolicyDecision{Decision: PolicyDecisionExecute,
		LockCeilingMS: 700, DecisionID: 9}); !v.Allowed || v.LockCeilingMS != 700 ||
		v.DecisionID != 9 {
		t.Fatalf("execute verdict = %+v", v)
	}
	parked := rollbackVerdictFor(ActionPolicyDecision{Decision: PolicyDecisionParked,
		BlockedReason: string(policy.ReasonNoTypedContract), DecisionID: 3})
	if parked.Allowed || !strings.HasPrefix(parked.Reason, rollbackNoContract) ||
		parked.DecisionID != 3 {
		t.Fatalf("no contract verdict = %+v", parked)
	}
	blocked := rollbackVerdictFor(ActionPolicyDecision{Decision: PolicyDecisionBlocked,
		BlockedReason: "replica_mutation"})
	if blocked.Allowed || !strings.Contains(blocked.Reason, "replica_mutation") ||
		!strings.HasPrefix(blocked.Reason, standingWithheld) {
		t.Fatalf("blocked verdict = %+v", blocked)
	}
}

// The gate judges the statement that runs: an index rollback recorded
// without CONCURRENTLY (a pg_get_indexdef rebuild) is classified, and
// sent, in the CONCURRENTLY form executeRollbackSQL runs.
func TestRollbackRequestJudgesTheStatementThatRuns(t *testing.T) {
	e := New(nil, wave1PolicyConfig("advisory"), time.Time{}, nopLog)
	req := e.rollbackRequest(context.Background(), 7,
		"CREATE INDEX t_ab ON public.t USING btree (a, b)", false)
	if req.Contract == nil || req.Contract.ActionType != "create_index_concurrently" ||
		!strings.HasPrefix(req.SQL, "CREATE INDEX CONCURRENTLY t_ab") {
		t.Fatalf("request = %+v contract %+v", req, req.Contract)
	}
	if !req.Rollback || req.OperatorApproved || req.Feature != string(policy.ChangeIndex) ||
		req.Evidence["rolled_back_action_id"] != int64(7) ||
		req.Evidence["source"] != "automatic_rollback" {
		t.Fatalf("request flags = %+v", req)
	}
	if len(req.TargetObjs) != 1 || req.TargetObjs[0] != "public.t" {
		t.Fatalf("targets = %v, want public.t", req.TargetObjs)
	}
	empty := e.rollbackRequest(context.Background(), 7, "", true)
	if empty.Contract != nil || !empty.Rollback || !empty.OperatorApproved {
		t.Fatalf("empty rollback request = %+v", empty)
	}
}
