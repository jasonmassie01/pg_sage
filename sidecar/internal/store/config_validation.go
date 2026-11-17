package store

import (
	"fmt"
	"strconv"

	"github.com/pg-sage/sidecar/internal/config"
)

// overridableSpec is the registry contract of a key the configuration API
// may store (config-02b): the api struct tag on the Config field.
func overridableSpec(key string) (config.KeySpec, error) {
	spec, ok := config.LookupKey(key)
	if !ok || !spec.Overridable {
		return config.KeySpec{}, fmt.Errorf("validate: unknown config key %q", key)
	}
	return spec, nil
}

func validateConfigKey(key string) error {
	_, err := overridableSpec(key)
	return err
}

func validateConfigValue(key, value string) error {
	spec, err := overridableSpec(key)
	if err != nil {
		return err
	}
	return validateSpec(spec, value)
}

// ValidateConfigOverride validates a single config override without
// persisting it. Handlers use this to make multi-key saves atomic.
func ValidateConfigOverride(key, value string) error {
	return validateConfigValue(key, value)
}

// validateSpec applies the key's registry range or enum, then the named
// special types' own grammars.
func validateSpec(spec config.KeySpec, value string) error {
	if spec.HasRange {
		return validateNumber(spec, value)
	}
	if err := spec.CheckEnum(value); err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	switch spec.Type {
	case "bool":
		return validateBool(spec.Key, value)
	case "bool_auto": // logwatch.enabled also takes auto (rca-09b)
		if _, err := config.ParseLogWatchEnabled(value); err != nil {
			return fmt.Errorf("validate: %w", err)
		}
		return nil
	case "clone_provider":
		return validateAPICloneProvider(value)
	case "maintenance_window":
		return validateMaintenanceWindow(value)
	case "rule_list":
		err := config.ValidateDisabledRules(spec.Key, config.ParseRuleList(value))
		if err != nil {
			return fmt.Errorf("validate: invalid value: %w", err)
		}
	}
	return nil
}

// validateAPICloneProvider refuses lakebase_branch, neon_branch,
// aurora_clone, rds_restore and cloudsql_clone on save: they need the
// clone.lakebase or clone.neon project binding (and Neon's API key), the
// clone.aurora or clone.rds region, or the clone.cloudsql project and
// instance, which are YAML or env only and restart-bound, so the API
// cannot switch to them.
func validateAPICloneProvider(value string) error {
	switch value {
	case "lakebase_branch":
		return fmt.Errorf("validate: clone.provider lakebase_branch is set in YAML " +
			"with its clone.lakebase project, not through the API")
	case config.CloneNeonBranch:
		return fmt.Errorf("validate: clone.provider neon_branch is set in YAML with its " +
			"clone.neon project and API key, not through the API")
	case config.CloneAuroraClone:
		return fmt.Errorf("validate: clone.provider aurora_clone is set in YAML with its " +
			"clone.aurora region, not through the API")
	case config.CloneRDSRestore:
		return fmt.Errorf("validate: clone.provider rds_restore is set in YAML with its " +
			"clone.rds region and clone.allow_full_copy, not through the API")
	case config.CloneCloudSQLClone:
		return fmt.Errorf("validate: clone.provider cloudsql_clone is set in YAML with " +
			"its clone.cloudsql project and instance and clone.allow_full_copy, not " +
			"through the API")
	}
	return nil
}

func validateNumber(spec config.KeySpec, value string) error {
	var n float64
	if spec.Integer {
		i, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("validate: %s must be an integer, got %q", spec.Key, value)
		}
		n = float64(i)
	} else {
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("validate: %s must be a number, got %q", spec.Key, value)
		}
		n = f
	}
	if err := spec.CheckOverride(n); err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	return nil
}

// validateMaintenanceWindow applies the policy window grammar so a typo is
// refused on save instead of being stored and silently meaning "never".
func validateMaintenanceWindow(value string) error {
	if err := config.ValidateMaintenanceWindow(value); err != nil {
		return fmt.Errorf("validate: invalid value: %w", err)
	}
	return nil
}

func validateBool(key, value string) error {
	if _, err := config.ParseBool(value); err != nil {
		return fmt.Errorf(
			"validate: %s must be true or false, got %q", key, value)
	}
	return nil
}
