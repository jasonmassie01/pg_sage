package api

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// perDatabaseKeys are the sage.config keys a database override applies
// live (execution_mode lives in sage.databases).
var perDatabaseKeys = map[string]bool{
	"trust.level":             true,
	"analyzer.disabled_rules": true, // market-11
}

// applyDatabaseWrite applies one persisted per-database override to the
// running instance.
func applyDatabaseWrite(inst *fleet.DatabaseInstance, write store.ConfigOverrideWrite,
	user *auth.User) {
	switch write.Key {
	case "trust.level":
		applyDatabaseTrustLevel(inst, write.Value)
	case "analyzer.disabled_rules":
		if inst.Analyzer != nil {
			inst.Analyzer.SetDisabledRules(config.ParseRuleList(write.Value),
				overrideActor(user))
		}
	}
}

// resetDatabaseOverride undoes a deleted per-database disabled-rules list:
// the database falls back to its YAML list, else the global one.
func resetDatabaseOverride(mgr *fleet.DatabaseManager, dbID int, key string) {
	if key != "analyzer.disabled_rules" || mgr == nil {
		return
	}
	inst := mgr.GetInstanceByDatabaseID(dbID)
	if inst == nil || inst.Analyzer == nil {
		return
	}
	if rules := inst.ConfigSnapshot().DisabledRules; rules != nil {
		inst.Analyzer.SetDisabledRules(rules,
			"config databases[].disabled_rules")
		return
	}
	inst.Analyzer.ClearDisabledRules()
}

// overrideActor names who set an override, for the findings it resolves.
func overrideActor(user *auth.User) string {
	switch {
	case user == nil:
		return "api"
	case user.Email != "":
		return user.Email
	default:
		return fmt.Sprintf("user %d", user.ID)
	}
}
