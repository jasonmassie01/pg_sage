package executor

import (
	"context"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Fast elevation reaches the gate and load admission: the configured
// trust ramp rides on every runtime snapshot, and verify.io_baseline_hours
// (when set) is the learned-baseline observation admission waits for.

func TestStandingRuntimeStateCarriesConfiguredRamp(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Trust.RampSafeHours, cfg.Trust.RampModerateHours = 1, 4
	exec := New(nil, cfg, time.Now(), noopExecLog)
	exec.emergencyStopFn = func(context.Context) bool { return false }

	state := exec.standingRuntimeState(context.Background(), policy.ActionRequest{})

	if state.SafeRampAge != time.Hour || state.ModerateRampAge != 4*time.Hour {
		t.Fatalf("runtime ramp = %s/%s, want 1h/4h", state.SafeRampAge, state.ModerateRampAge)
	}
}

func TestStandingRuntimeStateDefaultsToTheSpecRamp(t *testing.T) {
	exec := New(nil, config.DefaultConfig(), time.Now(), noopExecLog)
	exec.emergencyStopFn = func(context.Context) bool { return false }
	state := exec.standingRuntimeState(context.Background(), policy.ActionRequest{})
	if state.SafeRampAge != policy.SpecSafeRampAge ||
		state.ModerateRampAge != policy.SpecModerateRampAge {
		t.Fatalf("default runtime ramp = %s/%s, want the spec 8d/31d", state.SafeRampAge,
			state.ModerateRampAge)
	}
}

// Without a config the snapshot carries no ramp (zero), which the gate
// treats as the spec ramp; it never reads as "no ramp".
func TestStandingRuntimeStateWithoutConfigCarriesNoRamp(t *testing.T) {
	exec := New(nil, nil, time.Now(), noopExecLog)
	exec.emergencyStopFn = func(context.Context) bool { return false }
	state := exec.standingRuntimeState(context.Background(), policy.ActionRequest{})
	if state.SafeRampAge != 0 || state.ModerateRampAge != 0 {
		t.Fatalf("nil-config ramp = %s/%s, want zero", state.SafeRampAge,
			state.ModerateRampAge)
	}
}

func TestVerificationOptionsUseBaselineHours(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Verify.IOBaselineHours = 2
	if got := verificationOptions(cfg).BaselineDays; math.Abs(got-2.0/24) > 1e-12 {
		t.Fatalf("baseline = %v days, want 2 hours", got)
	}
	cfg.Verify.IOBaselineDays = 0 // hours still win
	if got := verificationOptions(cfg).BaselineDays; math.Abs(got-2.0/24) > 1e-12 {
		t.Fatalf("baseline with days 0 = %v days, want 2 hours", got)
	}
	cfg.Verify.IOBaselineHours = 0
	if got := verificationOptions(cfg).BaselineDays; got != 0 {
		t.Fatalf("both unset = %v, want disabled", got)
	}
}

func TestIndexAdmissionStatusReportsTheEffectiveBaseline(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Verify.IOBaselineDays, cfg.Verify.IOBaselineHours = 0, 6
	e := New(nil, cfg, zeroTime(), noopExecLog)
	status := e.IndexAdmissionStatus(context.Background())
	if math.Abs(status.BaselineRequiredDays-0.25) > 1e-12 {
		t.Fatalf("required baseline = %v days, want 0.25", status.BaselineRequiredDays)
	}
	if slices.Contains(status.MissingEvidence, "io_capacity_or_baseline") {
		t.Fatalf("an hour-scale baseline reported as missing: %v", status.MissingEvidence)
	}
	cfg.Verify.IOBaselineHours = 0
	status = e.IndexAdmissionStatus(context.Background())
	if !slices.Contains(status.MissingEvidence, "io_capacity_or_baseline") ||
		status.BaselineRequiredDays != 0 {
		t.Fatalf("disabled baseline status = %+v", status)
	}
}
