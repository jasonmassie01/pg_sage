package store

import (
	"strings"
	"testing"
)

// lab-05g: azure_restore needs its YAML-only clone.azure subscription,
// resource group and server and the clone.allow_full_copy opt-in, so the
// API cannot switch clone.provider to it.
func TestValidateConfigValue_RefusesAzureRestoreThroughTheAPI(t *testing.T) {
	err := validateConfigValue("clone.provider", "azure_restore")
	if err == nil || !strings.Contains(err.Error(), "clone.azure") ||
		!strings.Contains(err.Error(), "YAML") {
		t.Fatalf("azure_restore err = %v, want a refusal pointing at YAML", err)
	}
}
