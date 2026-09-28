package store

import (
	"context"
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

// Overrides saved before validation existed may hold an unparseable
// window, which keeps the window closed. Startup names each one and how to
// fix it; the runtime stays fail-closed.
func TestInvalidMaintenanceWindowOverrideWarnings(t *testing.T) {
	warnings := InvalidMaintenanceWindowOverrides([]ConfigOverride{
		{Key: "trust.maintenance_window", Value: "weeknigths"},
		{Key: "trust.maintenance_window", Value: "nights", DatabaseID: 4},
		{Key: "trust.maintenance_window", Value: "0 2 * *", DatabaseID: 7},
		{Key: "trust.maintenance_window", Value: "never"},
		{Key: "trust.level", Value: "weeknigths"},
	})
	if len(warnings) != 2 {
		t.Fatalf("warnings = %q, want 2 (one per invalid window override)", warnings)
	}
	for _, want := range []string{"weeknigths", "global", "closed",
		"/api/v1/config/global/trust.maintenance_window"} {
		if !strings.Contains(warnings[0], want) {
			t.Errorf("global warning %q lacks %q", warnings[0], want)
		}
	}
	for _, want := range []string{"0 2 * *", "database 7",
		"/api/v1/config/databases/7/trust.maintenance_window"} {
		if !strings.Contains(warnings[1], want) {
			t.Errorf("per-database warning %q lacks %q", warnings[1], want)
		}
	}
}

func TestInvalidMaintenanceWindowOverridesEmpty(t *testing.T) {
	for _, overrides := range [][]ConfigOverride{nil, {}} {
		if got := InvalidMaintenanceWindowOverrides(overrides); len(got) != 0 {
			t.Fatalf("warnings for %v = %q, want none", overrides, got)
		}
	}
}

// The store reads every scope. A row written directly (as an older release
// could) is reported; the valid one is not.
func TestMaintenanceWindowOverrideWarningsReadsStore(t *testing.T) {
	pool, ctx := coverageDB(t)
	cleanup := func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.config WHERE key = 'trust.maintenance_window'`)
	}
	cleanup()
	t.Cleanup(cleanup)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.config (key, value)
		VALUES ('trust.maintenance_window', 'weeknigths')`); err != nil {
		t.Fatalf("seed override: %v", err)
	}

	warnings, err := NewConfigStore(pool).MaintenanceWindowOverrideWarnings(ctx)

	if err != nil {
		t.Fatalf("MaintenanceWindowOverrideWarnings: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "weeknigths") {
		t.Fatalf("warnings = %q, want one naming the stored value", warnings)
	}
}
