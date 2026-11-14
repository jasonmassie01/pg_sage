package config

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/custodian/freeze"
)

// vacuum-11b: custodian.freeze.red_buffer_pct is accepted only where the
// freeze custodian uses it as given, 0 < value < 100. Before, validation
// took (0, 100] and NaN, while freeze.BufferThresholds silently replaced
// 100 (and anything it judged out of range) with 25.

const redBufferRefusal = "custodian.freeze.red_buffer_pct must be greater than 0 " +
	"and less than 100"

func redBufferYAML(value string) string {
	return standaloneIOYAML + "custodian:\n  freeze:\n    red_buffer_pct: " + value + "\n"
}

func TestRedBufferPctLoadsInsideOpenInterval(t *testing.T) {
	tests := []struct {
		yaml string
		want float64
	}{
		{"25", 25},
		{"99.9", 99.9},
		{"99.999", 99.999}, // boundary: just under 100
		{"0.0001", 0.0001}, // boundary: just over 0
	}
	for _, tt := range tests {
		t.Run(tt.yaml, func(t *testing.T) {
			cfg, err := loadYAMLResult(t, redBufferYAML(tt.yaml))
			if err != nil {
				t.Fatalf("Load refused red_buffer_pct %s: %v", tt.yaml, err)
			}
			if got := cfg.Custodian.Freeze.RedBufferPct; got != tt.want {
				t.Fatalf("red_buffer_pct = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRedBufferPctRefusedOutsideOpenInterval(t *testing.T) {
	for _, value := range []string{"100", "150", "0", "-1", ".nan", "100.0001"} {
		t.Run(value, func(t *testing.T) {
			cfg, err := loadYAMLResult(t, redBufferYAML(value))
			if err == nil {
				t.Fatalf("Load accepted red_buffer_pct %s as %v", value,
					cfg.Custodian.Freeze.RedBufferPct)
			}
			if !strings.Contains(err.Error(), redBufferRefusal) {
				t.Fatalf("error = %q, want it to contain %q", err, redBufferRefusal)
			}
		})
	}
}

// The validator is the check every load path shares (file, reload, env);
// NaN reaches it from YAML (.nan) and from the environment.
func TestRedBufferPctValidatorRefusesNaNAndInfinity(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		cfg := DefaultConfig()
		cfg.Custodian.Freeze.RedBufferPct = value
		err := cfg.validateAgentNative()
		if err == nil || err.Error() != redBufferRefusal {
			t.Fatalf("validateAgentNative(red_buffer_pct=%v) = %v, want %q",
				value, err, redBufferRefusal)
		}
	}
}

func TestRedBufferPctEnvironmentOutsideIntervalRefused(t *testing.T) {
	tests := []struct{ value, want string }{
		{"100", redBufferRefusal},
		// The environment parser refuses a non-finite number before validation.
		{"NaN", "SAGE_CUSTODIAN_FREEZE_RED_BUFFER_PCT"},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			chdirTemp(t)
			t.Setenv("SAGE_DATABASE_URL", "postgres://u:p@db.example:5432/app")
			t.Setenv("SAGE_CUSTODIAN_FREEZE_RED_BUFFER_PCT", tt.value)
			_, err := Load(nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load with env red_buffer_pct %s = %v, want %q",
					tt.value, err, tt.want)
			}
		})
	}
}

// A quoted number is a YAML string, which a float field never decodes;
// that refusal is the decoder's, as before this change.
func TestRedBufferPctQuotedStringFailsDecoding(t *testing.T) {
	_, err := loadYAMLResult(t, redBufferYAML(`"25"`))
	if err == nil {
		t.Fatal("Load accepted a quoted red_buffer_pct")
	}
	if strings.Contains(err.Error(), redBufferRefusal) {
		t.Fatalf("error = %q, want a decoding error, not the range check", err)
	}
	if !strings.Contains(err.Error(), "float64") {
		t.Fatalf("error = %q, want the decoder's float64 type error", err)
	}
}

// Default masking: with the key absent, and with no config file at all,
// the value is the shipped 25, not zero.
func TestRedBufferPctAbsentKeyDefaultsTo25(t *testing.T) {
	cfg, err := loadYAMLResult(t, standaloneIOYAML)
	if err != nil {
		t.Fatalf("Load without the key: %v", err)
	}
	if got := cfg.Custodian.Freeze.RedBufferPct; got != 25 {
		t.Fatalf("red_buffer_pct without the key = %v, want 25", got)
	}

	chdirTemp(t)
	cfg, err = Load([]string{"--pg-url", "postgres://u:p@db.example:5432/app"})
	if err != nil {
		t.Fatalf("Load without a config file: %v", err)
	}
	if got := cfg.Custodian.Freeze.RedBufferPct; got != 25 {
		t.Fatalf("red_buffer_pct without a config file = %v, want 25", got)
	}
}

// Agreement: every value validation accepts reaches the custodian as
// given; none falls back to the default.
func TestRedBufferPctAcceptedValuesAreUsedByBufferThresholds(t *testing.T) {
	for _, value := range []float64{0.5, 1, 25, 49.9, 50, 99.9} {
		cfg := DefaultConfig()
		cfg.Custodian.Freeze.RedBufferPct = value
		if err := cfg.validateAgentNative(); err != nil {
			t.Fatalf("validation refused %v: %v", value, err)
		}
		got := freeze.BufferThresholds(value)
		if got.RedBufferPct != value {
			t.Fatalf("BufferThresholds(%v).RedBufferPct = %v; the custodian "+
				"would not use the validated value", value, got.RedBufferPct)
		}
		if want := math.Min(2*value, 100); got.AmberBufferPct != want {
			t.Fatalf("BufferThresholds(%v).AmberBufferPct = %v, want %v",
				value, got.AmberBufferPct, want)
		}
	}
}

// Agreement, the other direction: every value BufferThresholds replaces
// with its fallback is refused by validation, so no fallback is silent.
func TestRedBufferPctFallbackValuesAreRefused(t *testing.T) {
	for _, value := range []float64{0, -1, 100, 150, math.NaN()} {
		if got := freeze.BufferThresholds(value); got.RedBufferPct == value {
			t.Fatalf("BufferThresholds(%v) used the value; expected the fallback", value)
		}
		cfg := DefaultConfig()
		cfg.Custodian.Freeze.RedBufferPct = value
		if err := cfg.validateAgentNative(); err == nil {
			t.Fatalf("validation accepted %v, which the custodian replaces with 25", value)
		}
	}
}

// Error propagation and state transition through the reload path: a
// reload to 100 is refused and the running 25 kept (the callback never
// runs); a later reload to 40 is accepted and yields 40/80 thresholds.
func TestRedBufferPctReloadRejectsThenAccepts(t *testing.T) {
	initial := DefaultConfig()
	candidate := Clone(initial)
	candidate.Custodian.Freeze.RedBufferPct = 100
	calls := 0
	watcher := NewAcknowledgedWatcherWithLoader("config.yaml", initial,
		func() (*Config, error) { return Clone(candidate), nil },
		func(*Config) error { calls++; return nil })

	watcher.reload()
	if calls != 0 {
		t.Fatalf("onChange ran %d times for a refused candidate", calls)
	}
	if got := watcher.Current().Custodian.Freeze.RedBufferPct; got != 25 {
		t.Fatalf("running red_buffer_pct after refused reload = %v, want 25", got)
	}
	if err := candidate.validateEffective(); err == nil ||
		!strings.Contains(err.Error(), redBufferRefusal) {
		t.Fatalf("reload validation error = %v, want %q", err, redBufferRefusal)
	}

	candidate.Custodian.Freeze.RedBufferPct = 40
	watcher.reload()
	if calls != 1 {
		t.Fatalf("onChange calls = %d after an accepted reload, want 1", calls)
	}
	current := watcher.Current().Custodian.Freeze.RedBufferPct
	got := freeze.BufferThresholds(current)
	if got.RedBufferPct != 40 || got.AmberBufferPct != 80 {
		t.Fatalf("thresholds after reload to 40 = %+v, want 40/80", got)
	}
}

// A loader error is propagated as a kept configuration, never a zero one.
func TestRedBufferPctReloadLoaderErrorKeepsRunningValue(t *testing.T) {
	initial := DefaultConfig()
	initial.Custodian.Freeze.RedBufferPct = 30
	watcher := NewAcknowledgedWatcherWithLoader("config.yaml", initial,
		func() (*Config, error) { return nil, errors.New("disk read failed") },
		func(*Config) error { t.Fatal("onChange ran after a loader error"); return nil })
	watcher.reload()
	if got := watcher.Current().Custodian.Freeze.RedBufferPct; got != 30 {
		t.Fatalf("red_buffer_pct after loader error = %v, want 30", got)
	}
}

// No concurrent access tests: validation is pure, and reload concurrency
// is covered by the watcher's own tests.
