package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/verify"
)

// lab-02: the executor's verify.method auto reads the database's latest
// verifier back-test through the reader the runtime installs; without one
// auto stays in shadow. The host withhold the weekly back-test honours is
// the load guard's.

type fakeBacktest struct {
	b     verify.Backtest
	found bool
	err   error
}

func (f fakeBacktest) LatestBacktest(context.Context, string) (verify.Backtest, bool,
	error) {
	return f.b, f.found, f.err
}

func backtestExecutor(method string) *Executor {
	cfg := config.DefaultConfig()
	cfg.Verify.Method = method
	return New(nil, cfg, time.Now(), func(string, string, ...any) {})
}

func TestVerificationMethod_AutoReadsTheInstalledBacktest(t *testing.T) {
	ctx := context.Background()
	e := backtestExecutor(verify.MethodAuto)
	if got := e.verificationMethod(ctx); got != verify.MethodShadow {
		t.Fatalf("no reader: %s, want shadow", got)
	}
	e = backtestExecutor(verify.MethodAuto)
	e.WithBacktestReader(fakeBacktest{b: verify.Backtest{Passed: true,
		RanAt: time.Now().Add(-time.Hour)}, found: true})
	if got := e.verificationMethod(ctx); got != verify.MethodV2 {
		t.Fatalf("passing back-test: %s, want v2", got)
	}
	e = backtestExecutor(verify.MethodAuto)
	e.WithBacktestReader(fakeBacktest{b: verify.Backtest{Passed: false,
		RanAt: time.Now()}, found: true})
	if got := e.verificationMethod(ctx); got != verify.MethodShadow {
		t.Fatalf("failing back-test: %s, want shadow", got)
	}
	e = backtestExecutor(verify.MethodAuto)
	e.WithBacktestReader(fakeBacktest{err: errors.New("relation does not exist")})
	if got := e.verificationMethod(ctx); got != verify.MethodShadow {
		t.Fatalf("unreadable back-test: %s, want shadow", got)
	}
	e = backtestExecutor(verify.MethodV1)
	e.WithBacktestReader(fakeBacktest{b: verify.Backtest{Passed: true,
		RanAt: time.Now()}, found: true})
	if got := e.verificationMethod(ctx); got != verify.MethodV1 {
		t.Fatalf("verify.method v1 with a passing back-test: %s, want v1", got)
	}
}

type guardCPU struct{ reasons []string }

func (guardCPU) CurrentCPU(context.Context) (float64, error) { return 10, nil }
func (g guardCPU) HostWithhold(context.Context) []string     { return g.reasons }

type plainCPU struct{}

func (plainCPU) CurrentCPU(context.Context) (float64, error) { return 10, nil }

func TestHostWithhold_IsTheLoadGuards(t *testing.T) {
	ctx := context.Background()
	e := backtestExecutor(verify.MethodAuto)
	if got := e.HostWithhold(ctx); got != nil {
		t.Fatalf("no reader: %v, want none", got)
	}
	e.WithHostCPUReader(plainCPU{})
	if got := e.HostWithhold(ctx); got != nil {
		t.Fatalf("a CPU reader without a guard: %v, want none", got)
	}
	e.WithHostCPUReader(guardCPU{reasons: []string{"storage runway 2 days"}})
	if got := e.HostWithhold(ctx); len(got) != 1 || got[0] != "storage runway 2 days" {
		t.Fatalf("guard reasons = %v", got)
	}
	var nilExec *Executor
	if got := nilExec.HostWithhold(ctx); got != nil {
		t.Fatalf("nil executor: %v", got)
	}
}
