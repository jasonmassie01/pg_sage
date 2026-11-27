package config

import "testing"

// No concurrent access tests: MigrationLintRate is a pure read.

// Default value masking: unset (0) and negative values mean the default,
// 30 per minute; a set value is kept.
func TestMigrationLintRateDefaults(t *testing.T) {
	for in, want := range map[int]int{0: 30, -5: 30, 1: 1, 120: 120} {
		if got := (APIConfig{MigrationLintRatePerMinute: in}).MigrationLintRate(); got != want {
			t.Errorf("MigrationLintRate(%d) = %d, want %d", in, got, want)
		}
	}
}

// With no config file, the loaded default is 30.
func TestMigrationLintRateWithoutConfigFile(t *testing.T) {
	var c Config
	if got := c.API.MigrationLintRate(); got != DefaultMigrationLintRatePerMinute ||
		DefaultMigrationLintRatePerMinute != 30 {
		t.Fatalf("default %d", got)
	}
}
