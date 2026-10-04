package tuner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// C12: verify_after_apply=false disables the revalidation loop instead of
// being an inert key.
func TestStartRevalidationLoop_RespectsVerifyAfterApply(t *testing.T) {
	var logs []string
	logFn := func(_, msg string, args ...any) { logs = append(logs, msg) }
	tu := New(nil, TunerConfig{VerifyAfterApply: false}, nil, logFn)
	done := make(chan struct{})
	go func() {
		tu.StartRevalidationLoop(context.Background(), 1)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop kept running with verify_after_apply=false")
	}
	if !strings.Contains(strings.Join(logs, "\n"), "verify_after_apply") {
		t.Errorf("no log explaining the disabled loop: %v", logs)
	}
}

// G3-B16: agent hints keep the Set() allowlist and the work_mem clamp.
func TestProposeHint_SetAllowlistAndClamp(t *testing.T) {
	tu := New(nil, TunerConfig{WorkMemMaxMB: 512}, &HintPlanAvailability{Available: true,
		HintTableReady: true}, noopLog2)
	ctx := context.Background()
	accepted := map[string]string{
		`Set(work_mem "100000MB") HashJoin(a b)`:    `Set(work_mem "512MB") HashJoin(a b)`,
		`Set(work_mem "4GB")`:                       `Set(work_mem "512MB")`,
		`Set(plan_cache_mode "force_generic_plan")`: `Set(plan_cache_mode "force_generic_plan")`,
		`Set(work_mem "131072kB")`:                  `Set(work_mem "128MB")`,
	}
	qid := int64(100)
	for hint, want := range accepted {
		qid++
		f, err := tu.ProposeHint(ctx, HintProposal{QueryID: qid, Hint: hint})
		if err != nil || f.Detail["hint_directive"] != want {
			t.Errorf("%q: %v %v, want %q", hint, f.Detail["hint_directive"], err, want)
		}
	}
	for _, hint := range []string{`Set(statement_timeout "0")`, `Set(enable_seqscan off)`,
		`Set(geqo off) NestLoop(a b)`} {
		qid++
		if _, err := tu.ProposeHint(ctx, HintProposal{QueryID: qid, Hint: hint}); !errors.Is(
			err, ErrInvalidHint) {
			t.Errorf("%q: %v, want ErrInvalidHint", hint, err)
		}
	}
}
