package config

import (
	"strings"
	"testing"
)

// forecast-05: the forecaster's sequence rule is retired (the runway
// forecasts sequences). forecaster.sequence_warn_days and
// sequence_critical_days still load, each with one warning, and the rest
// of the forecaster block keeps its values.

// No concurrent access tests: Load reads one file into a fresh Config.

func TestRetiredForecasterSequenceKeyLoadsWithOneWarning(t *testing.T) {
	cfg, out, err := loadWithWarnings(t,
		"forecaster:\n  sequence_warn_days: 90\n  lookback_days: 21\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := strings.Count(out, "WARNING:"); got != 1 {
		t.Fatalf("warnings %q: %d, want exactly one", out, got)
	}
	if !strings.Contains(out, "forecaster.sequence_warn_days") ||
		!strings.Contains(out, "sre.runways") {
		t.Fatalf("warning %q does not name the key and its replacement", out)
	}
	if cfg.Forecaster.LookbackDays != 21 {
		t.Fatalf("lookback_days = %d, want 21: the rest of the block must load",
			cfg.Forecaster.LookbackDays)
	}
}

func TestRetiredForecasterSequenceKeysEachWarn(t *testing.T) {
	_, out, err := loadWithWarnings(t,
		"forecaster:\n  sequence_warn_days: 0\n  sequence_critical_days: 30\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if strings.Count(out, "forecaster.sequence_warn_days") != 1 ||
		strings.Count(out, "forecaster.sequence_critical_days") != 1 {
		t.Fatalf("warnings %q, want one per retired key", out)
	}
}

func TestForecasterWithoutRetiredKeysGivesNoWarning(t *testing.T) {
	for name, body := range map[string]string{
		"no forecaster":         "mode: standalone\n",
		"live keys only":        "forecaster:\n  lookback_days: 30\n",
		"runway keys unchanged": "sre:\n  runways:\n    sequence_critical_days: 7\n",
		"forecaster not a map":  "forecaster: ~\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, out, err := loadWithWarnings(t, body)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if strings.Contains(out, "sequence_") {
				t.Fatalf("warnings %q for a config without retired keys", out)
			}
		})
	}
}

// A misspelt key next to a retired one is still refused (strict decode).
func TestRetiredForecasterKeyDoesNotHideTypos(t *testing.T) {
	if _, _, err := loadWithWarnings(t,
		"forecaster:\n  sequence_warn_days: 90\n  lookback_dayz: 3\n"); err == nil {
		t.Fatal("a misspelt forecaster key loaded")
	}
}
