package executor

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// config-01: the operator's trust ceilings (tier3 switches, maintenance
// window, rollback and cascade bounds) are live policy. SetTrustCeiling
// replaces them on a running executor; every reader takes them from the
// policy snapshot, never from the startup config.

func ceilingTestExecutor(cfg *config.Config) *Executor {
	e := New(nil, cfg, time.Now().Add(-60*24*time.Hour), noopExecLog)
	e.emergencyStopFn = func(context.Context) bool { return false }
	e.SetExecutionMode("auto")
	return e
}

func ceilingFromTrust(t config.TrustConfig) TrustCeiling {
	return TrustCeiling{
		Tier3Safe: t.Tier3Safe, Tier3Moderate: t.Tier3Moderate,
		MaintenanceWindow:     t.MaintenanceWindow,
		RollbackThresholdPct:  t.RollbackThresholdPct,
		RollbackWindowMinutes: t.RollbackWindowMinutes,
		RollbackCooldownDays:  t.RollbackCooldownDays,
		CascadeCooldownCycles: t.CascadeCooldownCycles,
	}
}

func TestTrustCeilingLiveDowngradeReachesStandingRuntimeState(t *testing.T) {
	cfg := windowedAutonomousConfig()
	e := ceilingTestExecutor(cfg)
	before := e.standingRuntimeState(context.Background(), policy.ActionRequest{})
	if !before.Tier3Safe || !before.Tier3Moderate {
		t.Fatalf("seeded state = safe %t moderate %t, want both on",
			before.Tier3Safe, before.Tier3Moderate)
	}
	lowered := ceilingFromTrust(cfg.Trust)
	lowered.Tier3Safe = false
	e.SetTrustCeiling(&lowered)
	after := e.standingRuntimeState(context.Background(), policy.ActionRequest{})
	if after.Tier3Safe {
		t.Fatal("tier3_safe=false did not reach the next runtime state")
	}
	if !after.Tier3Moderate || after.TrustLevel != "autonomous" {
		t.Fatalf("unrelated bounds moved: moderate %t trust %q",
			after.Tier3Moderate, after.TrustLevel)
	}
	if !cfg.Trust.Tier3Safe {
		t.Fatal("SetTrustCeiling mutated the shared process config")
	}
	e.SetTrustCeiling(nil)
	if cleared := e.standingRuntimeState(context.Background(),
		policy.ActionRequest{}); !cleared.Tier3Safe {
		t.Fatal("clearing the ceiling did not fall back to the config value")
	}
}

func TestTrustCeilingKeepsTrustLevelOverride(t *testing.T) {
	e := ceilingTestExecutor(windowedAutonomousConfig())
	if err := e.SetTrustLevel("advisory"); err != nil {
		t.Fatal(err)
	}
	ceiling := ceilingFromTrust(windowedAutonomousConfig().Trust)
	ceiling.Tier3Moderate = false
	e.SetTrustCeiling(&ceiling)
	state := e.standingRuntimeState(context.Background(), policy.ActionRequest{})
	if state.TrustLevel != "advisory" || state.Tier3Moderate {
		t.Fatalf("state = trust %q moderate %t, want advisory and moderate off",
			state.TrustLevel, state.Tier3Moderate)
	}
}

func TestTrustCeilingNarrowedWindowClosesOnNextCall(t *testing.T) {
	cfg := windowedAutonomousConfig() // maintenance_window: always
	e := ceilingTestExecutor(cfg)
	if state := e.standingRuntimeState(context.Background(),
		policy.ActionRequest{}); !state.InConfiguredWindow || !state.WindowConfigured {
		t.Fatalf("always window: in=%t configured=%t", state.InConfiguredWindow,
			state.WindowConfigured)
	}
	narrowed := ceilingFromTrust(cfg.Trust)
	narrowed.MaintenanceWindow = "never"
	e.SetTrustCeiling(&narrowed)
	state := e.standingRuntimeState(context.Background(), policy.ActionRequest{})
	if state.InConfiguredWindow {
		t.Fatal("window narrowed to never is still open on the next call")
	}
	if !state.WindowConfigured {
		t.Fatal("a never window must still count as configured (closed)")
	}
}

func TestTrustCeilingDowngradeRefusesSafeAnalyzeAtTheGate(t *testing.T) {
	e := withTestStandingGate(ceilingTestExecutor(windowedAutonomousConfig()))
	contract := riskContract("safe")
	ctx := context.Background()
	if got := e.ExplainFamilies(ctx, []ActionContract{contract}, false)[0]; got.Decision !=
		PolicyDecisionExecute {
		t.Fatalf("before downgrade: %s (%s), want execute", got.Decision, got.BlockedReason)
	}
	lowered := ceilingFromTrust(windowedAutonomousConfig().Trust)
	lowered.Tier3Safe = false
	e.SetTrustCeiling(&lowered)
	got := e.ExplainFamilies(ctx, []ActionContract{contract}, false)[0]
	if got.Decision != PolicyDecisionBlocked ||
		got.BlockedReason != string(policy.ReasonTrustRampNotSatisfied) {
		t.Fatalf("after downgrade: %s (%s), want blocked by the tier ceiling",
			got.Decision, got.BlockedReason)
	}
}

func TestTrustCeilingRaiseTakesEffectOnNextEvaluation(t *testing.T) {
	cfg := windowedAutonomousConfig()
	cfg.Trust.Tier3Moderate = false
	e := withTestStandingGate(ceilingTestExecutor(cfg))
	contract := riskContract("moderate")
	ctx := context.Background()
	if got := e.ExplainFamilies(ctx, []ActionContract{contract}, false)[0]; got.Decision ==
		PolicyDecisionExecute {
		t.Fatal("moderate executed with tier3_moderate off")
	}
	raised := ceilingFromTrust(cfg.Trust)
	raised.Tier3Moderate = true
	e.SetTrustCeiling(&raised)
	if got := e.ExplainFamilies(ctx, []ActionContract{contract}, false)[0]; got.Decision !=
		PolicyDecisionExecute {
		t.Fatalf("after raise: %s (%s), want execute", got.Decision, got.BlockedReason)
	}
}

func TestTrustCeilingRebindsEveryNumericReader(t *testing.T) {
	cfg := windowedAutonomousConfig()
	cfg.Collector.IntervalSeconds = 60
	cfg.Trust.CascadeCooldownCycles = 1
	cfg.Trust.RollbackThresholdPct = 10
	cfg.Trust.RollbackWindowMinutes = 15
	cfg.Trust.RollbackCooldownDays = 1
	e := ceilingTestExecutor(cfg)
	if got := e.cascadeCooldown(); got != time.Minute {
		t.Fatalf("seed cascade cooldown = %v, want 1m", got)
	}
	ceiling := TrustCeiling{Tier3Safe: true, Tier3Moderate: true,
		MaintenanceWindow: "always", RollbackThresholdPct: 25,
		RollbackWindowMinutes: 90, RollbackCooldownDays: 7, CascadeCooldownCycles: 10}
	e.SetTrustCeiling(&ceiling)
	if got := e.cascadeCooldown(); got != 10*time.Minute {
		t.Fatalf("cascade cooldown = %v, want 10m from the live ceiling", got)
	}
	monitor := e.rollbackMonitorConfig(func(context.Context, string) bool { return true })
	if monitor.ThresholdPct != 25 || monitor.WindowMinutes != 90 {
		t.Fatalf("rollback monitor = %d%%/%dm, want 25%%/90m",
			monitor.ThresholdPct, monitor.WindowMinutes)
	}
	if got := e.VerificationWaits().windows().RollbackWindow; got != 90*time.Minute {
		t.Fatalf("verification rollback window = %v, want 90m", got)
	}
	if got := e.rollbackCooldownDays(); got != 7 {
		t.Fatalf("rollback cooldown days = %d, want 7", got)
	}
	if got := e.TrustCeiling(); got != ceiling {
		t.Fatalf("TrustCeiling() = %+v, want %+v", got, ceiling)
	}
}

func TestTrustCeilingCascadeGuardFollowsLiveCooldown(t *testing.T) {
	cfg := windowedAutonomousConfig()
	cfg.Collector.IntervalSeconds = 60
	cfg.Trust.CascadeCooldownCycles = 1
	e := ceilingTestExecutor(cfg)
	e.recentActions["public.orders"] = time.Now().Add(-2 * time.Minute)
	if e.isCascadeCooldown("public.orders") {
		t.Fatal("2m-old action is inside a 1m cooldown")
	}
	ceiling := ceilingFromTrust(cfg.Trust)
	ceiling.CascadeCooldownCycles = 5
	e.SetTrustCeiling(&ceiling)
	if !e.isCascadeCooldown("public.orders") {
		t.Fatal("raised cascade cooldown did not hold the 2m-old action")
	}
}

func TestTrustCeilingReachesOperatorBound(t *testing.T) {
	e := ceilingTestExecutor(windowedAutonomousConfig())
	ceiling := ceilingFromTrust(windowedAutonomousConfig().Trust)
	ceiling.Tier3Safe, ceiling.Tier3Moderate = false, false
	e.SetTrustCeiling(&ceiling)
	if bound := e.OperatorBound(); bound.Tier3Safe || bound.Tier3Moderate {
		t.Fatalf("operator bound = safe %t moderate %t, want both off",
			bound.Tier3Safe, bound.Tier3Moderate)
	}
}

func TestTrustCeilingWithoutOverrideReportsConfig(t *testing.T) {
	cfg := windowedAutonomousConfig()
	cfg.Trust.RollbackCooldownDays = 4
	e := ceilingTestExecutor(cfg)
	if got, want := e.TrustCeiling(), ceilingFromTrust(cfg.Trust); got != want {
		t.Fatalf("TrustCeiling() = %+v, want config %+v", got, want)
	}
}

// Nil/zero: an executor without a config has no ceiling to report and the
// cascade guard keeps its documented fallback instead of dereferencing nil.
func TestTrustCeilingNilConfig(t *testing.T) {
	e := New(nil, nil, time.Time{}, noopExecLog)
	if got := e.TrustCeiling(); got != (TrustCeiling{}) {
		t.Fatalf("nil config ceiling = %+v, want zero", got)
	}
	e.SetTrustCeiling(nil)
	if got := e.cascadeCooldown(); got != 5*time.Minute {
		t.Fatalf("nil config cascade cooldown = %v, want 5m fallback", got)
	}
	ceiling := TrustCeiling{Tier3Safe: true}
	e.SetTrustCeiling(&ceiling)
	if got := e.TrustCeiling(); got != (TrustCeiling{}) {
		t.Fatalf("a ceiling without a config = %+v, want zero (fail closed)", got)
	}
}

func TestTrustCeilingSetterCopiesItsArgument(t *testing.T) {
	e := ceilingTestExecutor(windowedAutonomousConfig())
	ceiling := ceilingFromTrust(windowedAutonomousConfig().Trust)
	e.SetTrustCeiling(&ceiling)
	ceiling.Tier3Safe = false
	if !e.TrustCeiling().Tier3Safe {
		t.Fatal("caller mutation after SetTrustCeiling leaked into the executor")
	}
}

func TestStricterTrustCeiling(t *testing.T) {
	old := TrustCeiling{Tier3Safe: true, Tier3Moderate: false,
		MaintenanceWindow: "weeknights", RollbackThresholdPct: 10,
		RollbackWindowMinutes: 15, RollbackCooldownDays: 3, CascadeCooldownCycles: 2}
	next := TrustCeiling{Tier3Safe: false, Tier3Moderate: true,
		MaintenanceWindow: "always", RollbackThresholdPct: 50,
		RollbackWindowMinutes: 5, RollbackCooldownDays: 1, CascadeCooldownCycles: 1}
	got := StricterTrustCeiling(old, next)
	want := old
	want.Tier3Safe, want.Tier3Moderate = false, false
	if got != want {
		t.Fatalf("StricterTrustCeiling = %+v, want %+v", got, want)
	}
	if again := StricterTrustCeiling(next, old); again.Tier3Safe || again.Tier3Moderate {
		t.Fatalf("stricter is not symmetric for switches: %+v", again)
	}
}

// Concurrency: a ceiling swap racing many snapshots never yields a torn
// mix of two ceilings. (-race is unavailable on this machine; the check
// below still catches a non-atomic overlay.)
func TestTrustCeilingConcurrentSnapshotsAreNeverTorn(t *testing.T) {
	cfg := windowedAutonomousConfig()
	e := ceilingTestExecutor(cfg)
	a := TrustCeiling{Tier3Safe: true, Tier3Moderate: true, MaintenanceWindow: "always",
		RollbackThresholdPct: 10, RollbackWindowMinutes: 10, RollbackCooldownDays: 10,
		CascadeCooldownCycles: 10}
	b := TrustCeiling{MaintenanceWindow: "never", RollbackThresholdPct: 20,
		RollbackWindowMinutes: 20, RollbackCooldownDays: 20, CascadeCooldownCycles: 20}
	e.SetTrustCeiling(&a)
	stop := make(chan struct{})
	writer := alternateCeilings(e, a, b, stop)
	var readers sync.WaitGroup
	errs := make(chan error, 100)
	for i := 0; i < 100; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := 0; j < 50; j++ {
				snap, _, _ := e.policySnapshot()
				got := ceilingFromTrust(snap.Trust)
				if got != a && got != b {
					errs <- fmt.Errorf("torn snapshot %+v", got)
					return
				}
			}
		}()
	}
	readers.Wait()
	close(stop)
	writer.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// alternateCeilings swaps the executor between a and b until stop closes.
func alternateCeilings(e *Executor, a, b TrustCeiling, stop <-chan struct{}) *sync.WaitGroup {
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			next := a
			if i%2 == 1 {
				next = b
			}
			e.SetTrustCeiling(&next)
		}
	}()
	return &writer
}
