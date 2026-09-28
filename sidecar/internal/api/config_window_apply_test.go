package api

import (
	"strings"
	"testing"
)

// D2 T7: the config API refuses a window typo before persisting or
// hot-reloading it.
func TestConfigWritesRejectInvalidMaintenanceWindow(t *testing.T) {
	writes, errs := validatedConfigWrites(map[string]any{
		"trust.maintenance_window": "weeknigths",
	})
	if len(writes) != 0 {
		t.Fatalf("writes = %+v, want none", writes)
	}
	if len(errs) != 1 || !strings.Contains(errs[0], "trust.maintenance_window") ||
		!strings.Contains(errs[0], "weeknigths") {
		t.Fatalf("errs = %q, want one error naming the key and value", errs)
	}
	writes, errs = validatedConfigWrites(map[string]any{
		"trust.maintenance_window": "weeknights",
	})
	if len(errs) != 0 || len(writes) != 1 || writes[0].Value != "weeknights" {
		t.Fatalf("valid window: writes=%+v errs=%q", writes, errs)
	}
}
