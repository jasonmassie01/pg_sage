package store

import (
	"strings"
	"testing"
)

// D2 T7: a config save of an unparseable trust.maintenance_window is
// rejected (it used to be stored and silently mean "never").
func TestValidateConfigOverrideMaintenanceWindow(t *testing.T) {
	for _, ok := range []string{"", "never", "weeknights", "0 2 * * 1-5",
		"weekdays 01:00-05:00 America/Chicago"} {
		if err := ValidateConfigOverride("trust.maintenance_window", ok); err != nil {
			t.Errorf("ValidateConfigOverride(%q) = %v, want nil", ok, err)
		}
	}
	err := ValidateConfigOverride("trust.maintenance_window", "weeknigths")
	if err == nil || !strings.Contains(err.Error(), "weeknigths") {
		t.Fatalf("ValidateConfigOverride(typo) = %v, want an error quoting the value", err)
	}
}
