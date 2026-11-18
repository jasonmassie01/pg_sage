package store

import (
	"strings"
	"testing"
)

// lab-05b: neon_branch needs its clone.neon project and API key, which are
// YAML (or env) only, so the API cannot switch clone.provider to it.
func TestValidateConfigValue_RefusesNeonBranchThroughTheAPI(t *testing.T) {
	err := validateConfigValue("clone.provider", "neon_branch")
	if err == nil || !strings.Contains(err.Error(), "clone.neon") ||
		!strings.Contains(err.Error(), "YAML") {
		t.Fatalf("neon_branch err = %v, want a refusal pointing at YAML", err)
	}
	for _, ok := range []string{"none", "dle"} {
		if err := validateConfigValue("clone.provider", ok); err != nil {
			t.Fatalf("%s refused: %v", ok, err)
		}
	}
	if err := validateConfigValue("clone.provider", "neon"); err == nil {
		t.Fatal("an unknown provider was accepted")
	}
}
