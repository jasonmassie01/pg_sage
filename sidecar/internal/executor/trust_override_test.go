package executor

import (
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

func newTestCfg(level string) *config.Config {
	return &config.Config{
		Trust: config.TrustConfig{
			Level:             level,
			Tier3Safe:         true,
			MaintenanceWindow: "* * * * *",
		},
	}
}

func newTestExecutor(cfg *config.Config, rampStart time.Time) *Executor {
	return New(nil, cfg, nil, rampStart, func(string, string, ...any) {})
}

// safeFinding returns a Finding with ActionRisk "safe" and the minimum
// fields needed for ShouldExecute to evaluate it.
func safeFinding() analyzer.Finding {
	return analyzer.Finding{
		Category:         "index_health",
		Severity:         "warning",
		ObjectIdentifier: "public.test_table",
		Title:            "unused index",
		RecommendedSQL:   "DROP INDEX CONCURRENTLY idx_test",
		ActionRisk:       "safe",
	}
}

// --- TrustLevel() ---

func TestTrustLevel_ReturnsCfgWhenNoOverride(t *testing.T) {
	cfg := newTestCfg("advisory")
	e := newTestExecutor(cfg, time.Now())

	got := e.TrustLevel()
	if got != "advisory" {
		t.Errorf("TrustLevel() = %q, want %q", got, "advisory")
	}
}

func TestTrustLevel_ReturnsCfgObservation(t *testing.T) {
	cfg := newTestCfg("observation")
	e := newTestExecutor(cfg, time.Now())

	got := e.TrustLevel()
	if got != "observation" {
		t.Errorf("TrustLevel() = %q, want %q", got, "observation")
	}
}

func TestTrustLevel_ReturnsOverrideWhenSet(t *testing.T) {
	cfg := newTestCfg("observation")
	e := newTestExecutor(cfg, time.Now())

	_ = e.SetTrustLevel("autonomous")
	got := e.TrustLevel()
	if got != "autonomous" {
		t.Errorf("TrustLevel() = %q, want %q", got, "autonomous")
	}
}

func TestTrustLevel_OverrideSupersedesCfg(t *testing.T) {
	cfg := newTestCfg("advisory")
	e := newTestExecutor(cfg, time.Now())

	_ = e.SetTrustLevel("observation")
	got := e.TrustLevel()
	if got != "observation" {
		t.Errorf("TrustLevel() = %q, want %q after override",
			got, "observation")
	}
}

// --- SetTrustLevel("") clears override ---

func TestSetTrustLevel_EmptyClearsOverride(t *testing.T) {
	cfg := newTestCfg("advisory")
	e := newTestExecutor(cfg, time.Now())

	_ = e.SetTrustLevel("autonomous")
	if e.TrustLevel() != "autonomous" {
		t.Fatal("precondition: override should be active")
	}

	_ = e.SetTrustLevel("")
	got := e.TrustLevel()
	if got != "advisory" {
		t.Errorf("TrustLevel() after clear = %q, want %q (cfg value)",
			got, "advisory")
	}
}

func TestSetTrustLevel_EmptyOnFreshExecutor(t *testing.T) {
	cfg := newTestCfg("autonomous")
	e := newTestExecutor(cfg, time.Now())

	// Clearing override that was never set should be a no-op.
	_ = e.SetTrustLevel("")
	got := e.TrustLevel()
	if got != "autonomous" {
		t.Errorf("TrustLevel() = %q, want %q", got, "autonomous")
	}
}

// --- shouldExecute with override ---

func TestShouldExecute_OverrideUnblocks(t *testing.T) {
	// Config says "observation" which blocks everything.
	// Override to "advisory" with safe action + 30-day ramp should allow.
	cfg := newTestCfg("observation")
	rampStart := time.Now().Add(-30 * 24 * time.Hour)
	e := newTestExecutor(cfg, rampStart)

	f := safeFinding()

	// Without override: observation blocks all.
	if e.shouldExecute(f, false, false) {
		t.Error("shouldExecute with observation cfg should be false")
	}

	// With override to advisory: safe action + sufficient ramp.
	_ = e.SetTrustLevel("advisory")
	if !e.shouldExecute(f, false, false) {
		t.Error("shouldExecute with advisory override should be true " +
			"for safe action with 30-day ramp")
	}
}

func TestShouldExecute_OverrideToObservationBlocks(t *testing.T) {
	// Config says "advisory" (would allow), override to "observation"
	// should block.
	cfg := newTestCfg("advisory")
	rampStart := time.Now().Add(-30 * 24 * time.Hour)
	e := newTestExecutor(cfg, rampStart)

	f := safeFinding()

	// Without override: advisory allows safe actions.
	if !e.shouldExecute(f, false, false) {
		t.Fatal("precondition: advisory cfg should allow safe action")
	}

	_ = e.SetTrustLevel("observation")
	if e.shouldExecute(f, false, false) {
		t.Error("shouldExecute with observation override should be false")
	}
}

// --- shouldExecute without override (uses cfg) ---

func TestShouldExecute_NoCfgOverride_Advisory(t *testing.T) {
	cfg := newTestCfg("advisory")
	rampStart := time.Now().Add(-30 * 24 * time.Hour)
	e := newTestExecutor(cfg, rampStart)

	f := safeFinding()

	if !e.shouldExecute(f, false, false) {
		t.Error("shouldExecute with advisory cfg, safe action, " +
			"30-day ramp should be true")
	}
}

func TestShouldExecute_NoCfgOverride_InsufficientRamp(t *testing.T) {
	cfg := newTestCfg("advisory")
	rampStart := time.Now().Add(-1 * 24 * time.Hour) // only 1 day
	e := newTestExecutor(cfg, rampStart)

	f := safeFinding()

	if e.shouldExecute(f, false, false) {
		t.Error("shouldExecute with only 1-day ramp should be false " +
			"(advisory requires 8 days)")
	}
}

func TestShouldExecute_NoCfgOverride_EmergencyStop(t *testing.T) {
	cfg := newTestCfg("advisory")
	rampStart := time.Now().Add(-30 * 24 * time.Hour)
	e := newTestExecutor(cfg, rampStart)

	f := safeFinding()

	if e.shouldExecute(f, false, true) {
		t.Error("shouldExecute should be false during emergency stop")
	}
}

func TestShouldExecute_NoCfgOverride_Replica(t *testing.T) {
	cfg := newTestCfg("advisory")
	rampStart := time.Now().Add(-30 * 24 * time.Hour)
	e := newTestExecutor(cfg, rampStart)

	f := safeFinding()

	if e.shouldExecute(f, true, false) {
		t.Error("shouldExecute should be false on replica")
	}
}

// --- Override does not mutate original config ---

func TestShouldExecute_OverrideDoesNotMutateCfg(t *testing.T) {
	cfg := newTestCfg("observation")
	rampStart := time.Now().Add(-30 * 24 * time.Hour)
	e := newTestExecutor(cfg, rampStart)

	f := safeFinding()

	_ = e.SetTrustLevel("advisory")
	_ = e.shouldExecute(f, false, false)

	// The original config must still say "observation".
	if cfg.Trust.Level != "observation" {
		t.Errorf("original cfg.Trust.Level = %q, want %q; "+
			"shouldExecute mutated the config",
			cfg.Trust.Level, "observation")
	}
}

func TestShouldExecute_OverrideDoesNotMutateCfg_Concurrent(t *testing.T) {
	cfg := newTestCfg("observation")
	rampStart := time.Now().Add(-30 * 24 * time.Hour)

	f := safeFinding()
	const goroutines = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			// Each goroutine gets its own executor sharing the same cfg.
			e := newTestExecutor(cfg, rampStart)
			_ = e.SetTrustLevel("advisory")
			_ = e.shouldExecute(f, false, false)
		}()
	}

	wg.Wait()

	if cfg.Trust.Level != "observation" {
		t.Errorf("cfg.Trust.Level = %q after concurrent access, "+
			"want %q; concurrent shouldExecute corrupted config",
			cfg.Trust.Level, "observation")
	}
}

// --- Edge cases ---

func TestShouldExecute_OverrideWithModerateAction(t *testing.T) {
	// Advisory mode does not allow moderate-risk actions, even with
	// sufficient ramp and override.
	cfg := newTestCfg("observation")
	rampStart := time.Now().Add(-60 * 24 * time.Hour)
	e := newTestExecutor(cfg, rampStart)
	_ = e.SetTrustLevel("advisory")

	f := safeFinding()
	f.ActionRisk = "moderate"

	if e.shouldExecute(f, false, false) {
		t.Error("advisory override should not allow moderate actions")
	}
}

func TestShouldExecute_OverrideToAutonomousAllowsModerate(t *testing.T) {
	cfg := newTestCfg("observation")
	cfg.Trust.Tier3Moderate = true
	rampStart := time.Now().Add(-60 * 24 * time.Hour)
	e := newTestExecutor(cfg, rampStart)
	_ = e.SetTrustLevel("autonomous")

	f := safeFinding()
	f.ActionRisk = "moderate"

	// autonomous + moderate + Tier3Moderate + 60-day ramp +
	// maintenance window "* * * * *" = should execute.
	if !e.shouldExecute(f, false, false) {
		t.Error("autonomous override with moderate action, " +
			"Tier3Moderate=true, and 60-day ramp should execute")
	}
}

func TestShouldExecute_HighRiskAlwaysBlocked(t *testing.T) {
	cfg := newTestCfg("observation")
	cfg.Trust.Tier3HighRisk = true
	rampStart := time.Now().Add(-365 * 24 * time.Hour)
	e := newTestExecutor(cfg, rampStart)
	_ = e.SetTrustLevel("autonomous")

	f := safeFinding()
	f.ActionRisk = "high_risk"

	if e.shouldExecute(f, false, false) {
		t.Error("high_risk actions should always be blocked " +
			"regardless of override")
	}
}

func TestSetTrustLevel_MultipleOverrides(t *testing.T) {
	cfg := newTestCfg("observation")
	e := newTestExecutor(cfg, time.Now())

	levels := []string{"advisory", "autonomous", "observation", "advisory"}
	for _, lvl := range levels {
		_ = e.SetTrustLevel(lvl)
		got := e.TrustLevel()
		if got != lvl {
			t.Errorf("after SetTrustLevel(%q), TrustLevel() = %q",
				lvl, got)
		}
	}

	// Clear and verify fallback.
	_ = e.SetTrustLevel("")
	if e.TrustLevel() != "observation" {
		t.Errorf("after clearing, TrustLevel() = %q, want %q",
			e.TrustLevel(), "observation")
	}
}
