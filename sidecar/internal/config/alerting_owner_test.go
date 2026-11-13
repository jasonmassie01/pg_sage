package config

import "testing"

// alerting-06: the alerting owner applies the cooldown, quiet hours,
// timezone and repeat intervals to the running notification stack, so
// those fields are reconfigure; the legacy check interval stays
// restart-bound.
// No concurrent access tests: the registry is built once under sync.Once.

func TestAlertingPolicyFieldsReconfigureLive(t *testing.T) {
	for _, path := range []string{"alerting.cooldown_minutes", "alerting.quiet_hours_start",
		"alerting.quiet_hours_end", "alerting.timezone", "alerting.repeat_minutes"} {
		f, ok := LookupFieldLifecycle(path)
		if !ok || f.Lifecycle != LifecycleReconfigure || f.Owner != AlertingOwner {
			t.Errorf("%s = %+v, want reconfigure by %s", path, f, AlertingOwner)
		}
	}
	// alerting-06b moved enabled, routes, webhooks and the targets to the
	// owner (alerting_engine_test.go); the legacy interval stays here.
	for _, path := range []string{"alerting.check_interval_seconds"} {
		if f, _ := LookupFieldLifecycle(path); f.Lifecycle != LifecycleRestart || f.Owner != "" {
			t.Errorf("%s = %+v, want restart without owner", path, f)
		}
	}
}

func TestAlertingOwnerIsAProductionOwner(t *testing.T) {
	if AlertingOwner != "alerting" || !productionOwnerSet()[AlertingOwner] {
		t.Fatalf("AlertingOwner %q missing from ProductionOwners %v", AlertingOwner,
			ProductionOwners())
	}
}
