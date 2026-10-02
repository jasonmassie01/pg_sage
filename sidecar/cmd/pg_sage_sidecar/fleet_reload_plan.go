package main

import (
	"errors"
	"fmt"
	"sort"

	"github.com/pg-sage/sidecar/internal/config"
)

var (
	// errFleetControlDatabase refuses a reload that would remove or
	// reconnect the control database (auth, sessions, standing policy and
	// notification rules live there): that needs a restart.
	errFleetControlDatabase = errors.New("fleet control database cannot be " +
		"removed or reconnected by reload; restart pg_sage")
	// errInvalidFleetDatabase refuses a per-database policy value the
	// runtime cannot apply.
	errInvalidFleetDatabase = errors.New("invalid fleet database setting")
)

// fleetReloadPlan is the per-database difference between two YAML fleet
// configs. A renamed entry is a removal plus an addition.
type fleetReloadPlan struct {
	Add     []config.DatabaseConfig
	Remove  []string
	Rebuild []config.DatabaseConfig
	Hot     []config.DatabaseConfig
	// Previous holds the active config of every removed, rebuilt or hot
	// database, for rollback.
	Previous map[string]config.DatabaseConfig
}

// Empty reports whether the plan changes no database.
func (p fleetReloadPlan) Empty() bool {
	return len(p.Add)+len(p.Remove)+len(p.Rebuild)+len(p.Hot) == 0
}

// planFleetReload diffs the active and desired databases by name; every
// list is sorted by name.
func planFleetReload(active, desired []config.DatabaseConfig) fleetReloadPlan {
	plan := fleetReloadPlan{Previous: map[string]config.DatabaseConfig{}}
	current := make(map[string]config.DatabaseConfig, len(active))
	for _, db := range active {
		current[db.Name] = db
	}
	wanted := make(map[string]bool, len(desired))
	for _, db := range desired {
		wanted[db.Name] = true
		old, ok := current[db.Name]
		if !ok {
			plan.Add = append(plan.Add, db)
			continue
		}
		change := config.ClassifyDatabaseChange(old, db)
		switch {
		case change.Rebuild:
			plan.Rebuild = append(plan.Rebuild, db)
		case change.Changed():
			plan.Hot = append(plan.Hot, db)
		default:
			continue
		}
		plan.Previous[db.Name] = old
	}
	for name, old := range current {
		if !wanted[name] {
			plan.Remove = append(plan.Remove, name)
			plan.Previous[name] = old
		}
	}
	sortDatabases(plan.Add)
	sortDatabases(plan.Rebuild)
	sortDatabases(plan.Hot)
	sort.Strings(plan.Remove)
	return plan
}

func sortDatabases(dbs []config.DatabaseConfig) {
	sort.Slice(dbs, func(i, j int) bool { return dbs[i].Name < dbs[j].Name })
}

// validateFleetReloadPlan refuses plans that would retire the control
// database or carry a policy value the executor would reject. An empty
// control name (no database started at boot) protects nothing.
func validateFleetReloadPlan(plan fleetReloadPlan, controlName string) error {
	if controlName != "" {
		for _, name := range plan.Remove {
			if name == controlName {
				return fmt.Errorf("%w: removing %q", errFleetControlDatabase, name)
			}
		}
		for _, db := range plan.Rebuild {
			if db.Name == controlName {
				return fmt.Errorf("%w: reconnecting %q", errFleetControlDatabase, db.Name)
			}
		}
	}
	for _, group := range [][]config.DatabaseConfig{plan.Add, plan.Rebuild, plan.Hot} {
		for _, db := range group {
			if err := validateFleetDatabasePolicy(db); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateFleetDatabasePolicy(db config.DatabaseConfig) error {
	switch db.TrustLevel {
	case "", "observation", "advisory", "autonomous":
	default:
		return fmt.Errorf("%w: databases[%q].trust_level %q", errInvalidFleetDatabase,
			db.Name, db.TrustLevel)
	}
	switch db.ExecutionMode {
	case "", "auto", "approval", "manual":
	default:
		return fmt.Errorf("%w: databases[%q].execution_mode %q", errInvalidFleetDatabase,
			db.Name, db.ExecutionMode)
	}
	return nil
}
