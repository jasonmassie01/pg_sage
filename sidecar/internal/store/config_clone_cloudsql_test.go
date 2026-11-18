package store

import (
	"strings"
	"testing"
)

// lab-05e: cloudsql_clone needs its YAML-only clone.cloudsql project and
// instance and the clone.allow_full_copy opt-in, so the API cannot switch
// clone.provider to it.
func TestValidateConfigValue_RefusesCloudSQLCloneThroughTheAPI(t *testing.T) {
	err := validateConfigValue("clone.provider", "cloudsql_clone")
	if err == nil || !strings.Contains(err.Error(), "clone.cloudsql") ||
		!strings.Contains(err.Error(), "YAML") {
		t.Fatalf("cloudsql_clone err = %v, want a refusal pointing at YAML", err)
	}
}
