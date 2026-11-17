package executor

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/policy"
)

// lab-01c CHECK-08 (executor half): a step whose target runs once a week
// is unverifiable before it runs, so its request needs approval at every
// trust level with the reason on the card; no history is unknown and does
// not block; VACUUM is exempt.

const weeklyTarget = int64(7700770077)

func weeklyFinding() analyzer.Finding {
	return analyzer.Finding{Title: "index orders(status)",
		RecommendedSQL: "CREATE INDEX CONCURRENTLY orders_status_idx ON public.orders (status)",
		ObjectType:     "table", ObjectIdentifier: "public.orders",
		Detail: map[string]any{"queryid": weeklyTarget}}
}

// seedWeekly writes 4 weeks of one weekly run (500 calls) 100 h after now.
func seedWeekly(t *testing.T, f rollbackGateFixture, now time.Time) {
	t.Helper()
	for k := 1; k <= 4; k++ {
		at := now.Add(100*time.Hour - time.Duration(k)*7*24*time.Hour).Truncate(time.Hour)
		if _, err := f.pool.Exec(f.ctx, `INSERT INTO sage.query_rollup_hourly
			(queryid, hour, calls, exec_ms, rows, shared_blks_hit, shared_blks_read,
			 temp_blks_read, temp_blks_written, epoch_break, pairs)
			VALUES ($1, $2, 500, 10, 500, 100, 0, 0, 0, false, 60)`, weeklyTarget, at); err != nil {
			t.Fatalf("seed hourly: %v", err)
		}
	}
}

func TestV2_WeeklyTargetNeedsApprovalWithTheReason(t *testing.T) {
	f := newRollbackGateFixture(t)
	seedWeekly(t, f, time.Now())
	guarded := f.e.guardUnverifiable(f.ctx, weeklyFinding())
	reason := approvalRequiredReason(guarded)
	if !strings.Contains(reason, "cannot be verified within 72 h: the targets run 500 "+
		"times a week") {
		t.Fatalf("approval reason %q, want the card's unverifiable reason", reason)
	}
	ev, _ := guarded.Detail[DetailVerificationPreflight].(map[string]any)
	if ev["status"] != "unverifiable" {
		t.Fatalf("preflight evidence %v, want unverifiable", ev)
	}
	request := findingRequest(guarded, false)
	if request.Contract == nil || !containsGuardrail(request.Contract.Guardrails,
		policy.GuardrailApprovalRequired) {
		t.Fatalf("request guardrails %+v, want approval_required", request.Contract)
	}
	if _, ok := weeklyFinding().Detail[DetailVerificationPreflight]; ok {
		t.Fatal("the guard mutated the caller's finding detail")
	}
}

func TestV2_NoHistoryIsUnknownAndDoesNotBlock(t *testing.T) {
	f := newRollbackGateFixture(t)
	guarded := f.e.guardUnverifiable(f.ctx, weeklyFinding())
	if reason := approvalRequiredReason(guarded); reason != "" {
		t.Fatalf("no history blocked the step: %q", reason)
	}
	ev, _ := guarded.Detail[DetailVerificationPreflight].(map[string]any)
	if ev["status"] != "unknown" {
		t.Fatalf("preflight evidence %v, want unknown written to the evidence", ev)
	}
}

func TestV2_ImmediateClassesAreExempt(t *testing.T) {
	f := newRollbackGateFixture(t)
	seedWeekly(t, f, time.Now())
	vacuum := weeklyFinding()
	vacuum.RecommendedSQL = "VACUUM (ANALYZE) public.orders"
	guarded := f.e.guardUnverifiable(f.ctx, vacuum)
	if _, ok := guarded.Detail[DetailVerificationPreflight]; ok {
		t.Fatalf("VACUUM was preflighted: %v", guarded.Detail)
	}
}

// Error propagation: a history read that fails leaves the finding as it
// was (unknown, never inferred), and names the failure in the log.
func TestV2_PreflightReadFailureDoesNotBlock(t *testing.T) {
	f := newRollbackGateFixture(t)
	if _, err := f.pool.Exec(f.ctx, `ALTER TABLE sage.query_rollup_hourly
		RENAME TO query_rollup_hourly_gone`); err != nil {
		t.Fatalf("rename: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(f.ctx, `ALTER TABLE sage.query_rollup_hourly_gone
			RENAME TO query_rollup_hourly`)
	})
	var logged []string
	f.e.logFn = func(_, format string, args ...any) { logged = append(logged, format) }
	guarded := f.e.guardUnverifiable(f.ctx, weeklyFinding())
	if approvalRequiredReason(guarded) != "" || len(logged) == 0 ||
		!strings.Contains(logged[0], "preflight") {
		t.Fatalf("read failure: reason %q, log %v", approvalRequiredReason(guarded), logged)
	}
}

func containsGuardrail(gs []policy.Guardrail, g policy.Guardrail) bool {
	for _, x := range gs {
		if x == g {
			return true
		}
	}
	return false
}
