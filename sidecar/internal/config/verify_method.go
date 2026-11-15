package config

import (
	"fmt"
	"slices"
	"strings"
)

// Verification v2's switches (lab-01c). verify.method picks the verifier
// that decides retain or revert; verify.on_expired what happens to a
// monitor whose window ended while pg_sage was not watching.
const (
	DefaultVerifyMethod    = "auto"
	DefaultVerifyOnExpired = "ask"
)

// VerifyMethods and VerifyOnExpired are the accepted values.
var (
	VerifyMethods   = []string{"v1", "shadow", "v2", "auto"}
	VerifyOnExpired = []string{"ask", "keep", "revert"}
)

// validateMethod refuses a verifier or expiry policy outside the allowed
// values, naming them: an unknown value never falls back to a default.
func (v VerifyConfig) validateMethod() error {
	if !slices.Contains(VerifyMethods, v.Method) {
		return fmt.Errorf("verify.method must be one of %s, got %q",
			strings.Join(VerifyMethods, ", "), v.Method)
	}
	if !slices.Contains(VerifyOnExpired, v.OnExpired) {
		return fmt.Errorf("verify.on_expired must be one of %s, got %q",
			strings.Join(VerifyOnExpired, ", "), v.OnExpired)
	}
	return nil
}

// The verifier back-test's bounds (lab-02): a run's runtime in minutes and
// the weekly run's placebos; fewer than 200 could never pass the gate.
const (
	DefaultVerifyBacktestMaxRuntimeMinutes = 10
	DefaultVerifyBacktestPlacebos          = 300
	maxVerifyBacktestRuntimeMinutes        = 1440
	minVerifyBacktestPlacebos              = 200
	maxVerifyBacktestPlacebos              = 10000
)

// validateBacktest refuses a back-test bound outside its range.
func (v VerifyConfig) validateBacktest() error {
	if v.BacktestMaxRuntimeMinutes < 1 ||
		v.BacktestMaxRuntimeMinutes > maxVerifyBacktestRuntimeMinutes {
		return fmt.Errorf("verify.backtest_max_runtime_minutes must be 1-%d, got %d",
			maxVerifyBacktestRuntimeMinutes, v.BacktestMaxRuntimeMinutes)
	}
	if v.BacktestPlacebos < minVerifyBacktestPlacebos ||
		v.BacktestPlacebos > maxVerifyBacktestPlacebos {
		return fmt.Errorf("verify.backtest_placebos must be %d-%d, got %d",
			minVerifyBacktestPlacebos, maxVerifyBacktestPlacebos, v.BacktestPlacebos)
	}
	return nil
}
