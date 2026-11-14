package config

import "fmt"

// AlertingOwner is the reconfiguration owner of the alerting block: it
// applies the cooldown, quiet hours, timezone and repeat intervals to the
// running notification dispatchers and alerting managers (alerting-06),
// and with the notify engine reconciles the routes and targets into the
// notification tables (alerting-06b).
const AlertingOwner = "alerting"

// The alerting engines (alerting-06b). notify reconciles the alerting
// block into config-managed notification channels and rules; legacy runs
// the old per-database alerting manager until alerting-06d removes it.
const (
	AlertingEngineNotify = "notify"
	AlertingEngineLegacy = "legacy"
)

// EffectiveEngine is the engine the block runs on: notify unless legacy
// is set.
func (a *AlertingConfig) EffectiveEngine() string {
	if a.Engine == AlertingEngineLegacy {
		return AlertingEngineLegacy
	}
	return AlertingEngineNotify
}

// validateEngine refuses an engine other than notify or legacy, whether
// or not alerting is enabled: the switch is read once at startup.
func (a *AlertingConfig) validateEngine() error {
	switch a.Engine {
	case "", AlertingEngineNotify, AlertingEngineLegacy:
		return nil
	}
	return fmt.Errorf("alerting.engine %q is not notify or legacy", a.Engine)
}
