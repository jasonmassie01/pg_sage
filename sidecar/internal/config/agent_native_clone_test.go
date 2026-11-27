package config

import (
	"strings"
	"testing"
)

// migration-07: clone.provider snapshot has no adapter in this build, so a
// config naming it is refused at startup instead of silently disabling
// rehearsed apply_migration.
func TestCloneProviderSnapshotIsRefused(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Clone.Provider = "snapshot"
	err := cfg.validateAgentNative()
	if err == nil || !strings.Contains(err.Error(), "snapshot has no adapter in this build") {
		t.Fatalf("snapshot provider: %v", err)
	}
}

func TestCloneProviderAcceptedValues(t *testing.T) {
	for _, provider := range []string{"none", "dle"} {
		cfg := DefaultConfig()
		cfg.Clone.Provider = provider
		cfg.Clone.DLEEndpoint = "https://dle.invalid"
		if err := cfg.validateAgentNative(); err != nil {
			t.Errorf("%s: %v", provider, err)
		}
	}
	for _, provider := range []string{"", "Snapshot", "DLE", " dle"} {
		cfg := DefaultConfig()
		cfg.Clone.Provider = provider
		cfg.Clone.DLEEndpoint = "https://dle.invalid"
		err := cfg.validateAgentNative()
		if err == nil || !strings.Contains(err.Error(), "must be none or dle") {
			t.Errorf("%q: %v", provider, err)
		}
	}
}
