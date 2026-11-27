package main

import (
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// config-10: a fleet reload validates per-database trust against the one
// registry; an empty level inherits the global one.
func TestFleetDatabasePolicy_TrustAcceptsExactlyConfigTrustLevels(t *testing.T) {
	for _, level := range append(config.TrustLevels(), "") {
		db := config.DatabaseConfig{Name: "db1", TrustLevel: level}
		if err := validateFleetDatabasePolicy(db); err != nil {
			t.Errorf("trust_level %q: %v, want nil", level, err)
		}
	}
	for _, level := range []string{"Autonomous", "auto", "observe", " advisory"} {
		db := config.DatabaseConfig{Name: "db1", TrustLevel: level}
		err := validateFleetDatabasePolicy(db)
		if !errors.Is(err, errInvalidFleetDatabase) {
			t.Errorf("trust_level %q: error = %v, want errInvalidFleetDatabase", level, err)
		}
	}
}
