package store

import (
	"strings"
	"testing"
)

// lab-05d: rds_restore needs its YAML-only clone.rds region and the
// clone.allow_full_copy opt-in, so the API cannot switch clone.provider to
// it.
func TestValidateConfigValue_RefusesRDSRestoreThroughTheAPI(t *testing.T) {
	err := validateConfigValue("clone.provider", "rds_restore")
	if err == nil || !strings.Contains(err.Error(), "clone.rds") ||
		!strings.Contains(err.Error(), "YAML") {
		t.Fatalf("rds_restore err = %v, want a refusal pointing at YAML", err)
	}
}
