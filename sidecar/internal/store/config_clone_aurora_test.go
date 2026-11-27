package store

import (
	"strings"
	"testing"
)

// lab-05c: aurora_clone needs its YAML-only clone.aurora region, so the
// API cannot switch clone.provider to it.
func TestValidateConfigValue_RefusesAuroraCloneThroughTheAPI(t *testing.T) {
	err := validateConfigValue("clone.provider", "aurora_clone")
	if err == nil || !strings.Contains(err.Error(), "clone.aurora") ||
		!strings.Contains(err.Error(), "YAML") {
		t.Fatalf("aurora_clone err = %v, want a refusal pointing at YAML", err)
	}
	if err := validateConfigValue("clone.provider", "aurora"); err == nil {
		t.Fatal("an unknown provider was accepted")
	}
}
