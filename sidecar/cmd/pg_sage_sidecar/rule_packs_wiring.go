package main

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

const disabledRulesKey = "analyzer.disabled_rules"

// attachRulePacks gives the analyzer this database's owner checks and its
// own disabled-rules list (market-11): the stored API override wins over
// the YAML databases[].disabled_rules; neither inherits the global list.
func (rt *databaseRuntime) attachRulePacks() {
	checks, err := config.PrepareChecks(cfg.Checks)
	if err != nil {
		// Load already validated them; a failure here runs none (fail closed).
		logStructuredWrapper("ERROR", "custom checks not started on %s: %v", rt.spec.Name, err)
	} else {
		rt.analyzer.WithCustomChecks(checks)
	}
	if names := rt.spec.Config.DisabledRules; names != nil {
		rt.analyzer.SetDisabledRules(names, "config databases[].disabled_rules")
	}
	names, by, ok := rt.storedDisabledRules()
	if ok {
		rt.analyzer.SetDisabledRules(names, by)
	}
}

// storedDisabledRules reads this database's API override, if any. A row
// that no longer validates is skipped with the stored-override warning.
func (rt *databaseRuntime) storedDisabledRules() ([]string, string, bool) {
	if rt.spec.DatabaseID <= 0 || rt.spec.ControlPool == nil {
		return nil, "", false
	}
	ctx, cancel := context.WithTimeout(rt.ctx, 5*time.Second)
	defer cancel()
	overrides, err := store.NewConfigStore(rt.spec.ControlPool).GetOverrides(ctx,
		rt.spec.DatabaseID)
	if err != nil {
		logStructuredWrapper("WARN", "read the disabled rules of %s: %v; "+
			"it inherits the global list until the next change", rt.spec.Name, err)
		return nil, "", false
	}
	for _, o := range overrides {
		if o.Key != disabledRulesKey {
			continue
		}
		value, ok, problem := store.ReplayOverride(o.Key, o.Value)
		if !ok {
			logStructuredWrapper("WARN", "%s", store.StoredOverrideWarning(o, problem))
			return nil, "", false
		}
		return config.ParseRuleList(value), fmt.Sprintf("user %d", o.UpdatedBy), true
	}
	return nil, "", false
}
