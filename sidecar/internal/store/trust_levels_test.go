package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// config-10: the store validates trust against the one registry in config.

var notTrustLevels = []string{
	"", "Observation", "ADVISORY", "auto", "autonomous ", "manual", "approval", "observe",
}

func TestTrustOverride_AcceptsExactlyConfigTrustLevels(t *testing.T) {
	for _, level := range config.TrustLevels() {
		if err := ValidateConfigOverride("trust.level", level); err != nil {
			t.Errorf("trust.level %q rejected: %v", level, err)
		}
	}
	for _, level := range notTrustLevels {
		err := ValidateConfigOverride("trust.level", level)
		if err == nil {
			t.Errorf("trust.level %q accepted, want rejection", level)
			continue
		}
		if !strings.Contains(err.Error(), "must be one of") {
			t.Errorf("trust.level %q error = %q, want a must-be-one-of message", level, err)
		}
	}
}

func TestDatabaseInput_TrustAcceptsExactlyConfigTrustLevels(t *testing.T) {
	for _, level := range config.TrustLevels() {
		input := validInput()
		input.TrustLevel = level
		if err := validateInput(input, true); err != nil {
			t.Errorf("trust_level %q rejected: %v", level, err)
		}
	}
	for _, level := range notTrustLevels {
		input := validInput()
		input.TrustLevel = level
		err := validateInput(input, true)
		if !errors.Is(err, ErrValidation) {
			t.Errorf("trust_level %q: err = %v, want ErrValidation", level, err)
			continue
		}
		want := "trust_level must be one of " + strings.Join(config.TrustLevels(), ", ")
		if !strings.Contains(err.Error(), want) {
			t.Errorf("trust_level %q error = %q, want %q", level, err, want)
		}
	}
}

func TestTrustReplay_SkipsLevelsOutsideRegistry(t *testing.T) {
	for _, level := range config.TrustLevels() {
		got, ok, problem := ReplayOverride("trust.level", level)
		if !ok || got != level || problem != "" {
			t.Errorf("ReplayOverride(%q) = (%q, %v, %q), want applied as stored",
				level, got, ok, problem)
		}
	}
	got, ok, problem := ReplayOverride("trust.level", "auto")
	if ok || got != "" || problem == "" {
		t.Errorf("ReplayOverride(auto) = (%q, %v, %q), want skipped with a problem",
			got, ok, problem)
	}
}

// No concurrent access test: the validators read an immutable registry.
