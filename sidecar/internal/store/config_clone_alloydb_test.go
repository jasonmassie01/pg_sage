package store

import (
	"strings"
	"testing"
)

// lab-05f: alloydb_restore needs its YAML-only clone.alloydb project,
// location and cluster and the clone.allow_full_copy opt-in, so the API
// cannot switch clone.provider to it.
func TestValidateConfigValue_RefusesAlloyDBRestoreThroughTheAPI(t *testing.T) {
	err := validateConfigValue("clone.provider", "alloydb_restore")
	if err == nil || !strings.Contains(err.Error(), "clone.alloydb") ||
		!strings.Contains(err.Error(), "YAML") {
		t.Fatalf("alloydb_restore err = %v, want a refusal pointing at YAML", err)
	}
}
