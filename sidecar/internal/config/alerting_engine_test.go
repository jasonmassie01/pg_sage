package config

import "testing"

// alerting-06b: alerting.engine picks the one notification engine. The
// default notify reconciles the YAML block into notify channels and rules;
// legacy keeps the old per-database manager for one release. The engine is
// restart-scoped; with notify, routes and targets reconcile live.
// No concurrent access tests: validation and lifecycle lookups are pure.

const alertEngineBlock = "  enabled: true\n" + alertSlackURL +
	"  routes:\n    - severity: critical\n      channels: [slack]\n"

func TestAlertingEngine_DefaultIsNotify(t *testing.T) {
	cfg, err := loadAlertingYAML(t, alertEngineBlock)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Alerting.Engine != AlertingEngineNotify ||
		cfg.Alerting.EffectiveEngine() != AlertingEngineNotify {
		t.Fatalf("engine = %q (effective %q), want notify",
			cfg.Alerting.Engine, cfg.Alerting.EffectiveEngine())
	}
	var zero AlertingConfig
	if zero.EffectiveEngine() != AlertingEngineNotify {
		t.Fatalf("zero-value engine = %q, want notify", zero.EffectiveEngine())
	}
}

func TestAlertingEngine_LegacyIsAccepted(t *testing.T) {
	cfg, err := loadAlertingYAML(t, alertEngineBlock+"  engine: legacy\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Alerting.EffectiveEngine() != AlertingEngineLegacy {
		t.Fatalf("engine = %q, want legacy", cfg.Alerting.EffectiveEngine())
	}
}

func TestAlertingEngine_UnknownValueFailsNamingTheKey(t *testing.T) {
	requireAlertingErr(t, alertEngineBlock+"  engine: pagerduty\n",
		"alerting.engine", `"pagerduty"`, "notify", "legacy")
	// Checked even when alerting is off: the switch is read at startup.
	requireAlertingErr(t, "  engine: Notify\n", "alerting.engine", `"Notify"`)
}

func TestAlertingEngine_IsRestartScoped(t *testing.T) {
	f, ok := LookupFieldLifecycle("alerting.engine")
	if !ok || f.Lifecycle != LifecycleRestart || f.Owner != "" {
		t.Fatalf("alerting.engine = %+v, %v; want restart without owner", f, ok)
	}
}

func TestAlertingTargetsAndRoutesReconcileLive(t *testing.T) {
	for _, path := range []string{"alerting.enabled", "alerting.routes",
		"alerting.webhooks", "alerting.slack_webhook_url", "alerting.pagerduty_routing_key"} {
		f, ok := LookupFieldLifecycle(path)
		if !ok || f.Lifecycle != LifecycleReconfigure || f.Owner != AlertingOwner {
			t.Errorf("%s = %+v, want reconfigure by %s", path, f, AlertingOwner)
		}
	}
	if f, _ := LookupFieldLifecycle("alerting.check_interval_seconds"); f.Lifecycle !=
		LifecycleRestart {
		t.Errorf("alerting.check_interval_seconds = %+v, want restart (legacy only)", f)
	}
}
