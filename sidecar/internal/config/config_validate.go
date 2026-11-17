package config

import (
	"errors"
	"fmt"
	"os"
)

// validate runs the section validators in a fixed order and returns the
// first error. The order decides which error an operator sees first when
// several settings are wrong, so a new check goes where it belongs in it.
func (c *Config) validate() error {
	return firstError(
		c.validateAccess,
		c.validateCollector,
		c.validateAnalyzer,
		c.validateFeatures,
		c.validateChangeControl,
		c.validateSafety,
		c.validateResourceUse,
		c.validateAnalyzerLimits,
		c.validateLifecycle,
		c.validateOperations,
		c.validateFleetDatabases,
	)
}

// firstError runs checks in order and returns the first error.
func firstError(checks ...func() error) error {
	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

// validateAccess checks how pg_sage is reached and connects.
func (c *Config) validateAccess() error {
	return firstError(
		c.validateTLS,
		func() error { return c.API.validateTrustedOrigins() },
		c.validateAgentNative,
		c.validateAuthz,
		c.validatePostgresPort,
		c.validateActionIdentity,
	)
}

func (c *Config) validateCollector() error {
	if c.Collector.IntervalSeconds <= 0 {
		return fmt.Errorf("collector.interval_seconds must be positive")
	}
	if c.Collector.BatchSize <= 0 {
		return fmt.Errorf("collector.batch_size must be positive")
	}
	if c.Collector.MaxQueries <= 0 {
		return fmt.Errorf("collector.max_queries must be positive")
	}
	return nil
}

func (c *Config) validateAnalyzer() error {
	if c.Analyzer.IntervalSeconds <= 0 {
		return fmt.Errorf("analyzer.interval_seconds must be positive")
	}
	if c.Analyzer.SlowQueryThresholdMs < 0 {
		return fmt.Errorf("analyzer.slow_query_threshold_ms must be non-negative")
	}
	if c.Analyzer.SlowQueryMinCalls < 0 {
		return fmt.Errorf("analyzer.slow_query_min_calls must be non-negative, got %d",
			c.Analyzer.SlowQueryMinCalls)
	}
	return firstError(
		func() error { return c.Analyzer.validateSchemaGuard() },
		func() error { return c.Analyzer.validateTableBloat() },
		func() error { return c.Analyzer.validateIndexBloat() },
	)
}

// validateFeatures checks the budgets and the feature sections.
func (c *Config) validateFeatures() error {
	return firstError(
		func() error { return validateBudgets(c.Budgets) },
		func() error { return c.Briefing.validate() },
		func() error { return c.RCA.validate() },
		func() error { return c.SRE.validate() },
		func() error { return c.Ask.validate() },
		func() error { return c.Review.validate() },
		func() error { return c.Forecaster.validate() },
		func() error { return c.Facts.validate() },
		c.validateRulePacks,
		func() error { return c.Specialist.validate() },
		func() error { return c.LLM.validateWire() },
	)
}

// validateChangeControl checks trust, migration rehearsal and elevation.
func (c *Config) validateChangeControl() error {
	return firstError(c.validateTrust, c.validateMigrationRehearsal, c.validateElevation)
}

func (c *Config) validateSafety() error {
	if c.Safety.CPUCeilingPct <= 0 || c.Safety.CPUCeilingPct > 100 {
		return fmt.Errorf("safety.cpu_ceiling_pct must be 1-100")
	}
	if c.Safety.QueryTimeoutMs <= 0 {
		return fmt.Errorf("safety.query_timeout_ms must be positive")
	}
	return c.Safety.validateClassTimeouts()
}

// validateResourceUse checks IO admission, cloud telemetry, cost and the
// size of pg_sage's own schema.
func (c *Config) validateResourceUse() error {
	return firstError(
		c.validateIOAdmission,
		func() error { return c.LakebaseObservability.resolveAndValidate(os.Getenv) },
		c.validateCloudTelemetry,
		c.validateCost,
		func() error { return c.Retention.validateSageFootprint() },
	)
}

// validateAnalyzerLimits checks the analyzer thresholds, replication and
// pg_sage's own cost budgets.
func (c *Config) validateAnalyzerLimits() error {
	return firstError(
		func() error { return c.Analyzer.validateInactiveSlotMinMinutes() },
		func() error { return c.Analyzer.validateReplicationMinutes() },
		func() error { return c.Replication.validate() },
		func() error { return c.Analyzer.validateWraparoundFreezeShare() },
		func() error { return c.Analyzer.validateSelfCostBudget() },
		func() error { return c.SelfBudget.validate() },
		func() error { return c.Baseline.validate() },
		c.validateHistory,
	)
}

// validateLifecycle checks tuning, retention and self-configuration.
func (c *Config) validateLifecycle() error {
	return firstError(
		func() error { return c.LLM.Optimizer.RejectionMemory.validate() },
		func() error { return c.Tuning.validate() },
		func() error { return c.Tuner.validateParamShapeMinVariants() },
		func() error { return c.Tuner.validateHintCostTolerancePct() },
		func() error { return c.Retention.validateDecisionsDays() },
		func() error { return c.Retention.validateAuthAuditDays() },
		func() error { return c.Retention.validateStorage() },
		func() error { return c.SelfConfig.validate() },
	)
}

// validateOperations checks fleet learning and discovery, HA, backup,
// agents, audit, MCP, replicas, SLO owners, metrics and the value registry.
func (c *Config) validateOperations() error {
	return firstError(
		c.validateFleetLearning,
		func() error { return c.Fleet.Discovery.Validate() },
		func() error { return c.HA.validate() },
		func() error { return c.Backup.validate() },
		func() error { return c.Drill.validate() },
		c.validateDrillCommand,
		func() error { return c.Agents.validate() },
		func() error { return c.Audit.validate() },
		func() error { return c.MCP.OAuth.validate(c.MCP.Transport) },
		func() error { return validateReplicas(c.Databases) },
		c.validateSLOOwners,
		func() error { return c.Prometheus.validate() },
		c.validateRegistry,
	)
}

// validateFleetDatabases checks the fleet-mode database list.
func (c *Config) validateFleetDatabases() error {
	if c.Mode != "fleet" {
		return nil
	}
	if len(c.Databases) == 0 && !c.HasMetaDB() {
		return fmt.Errorf("fleet mode requires at least one database")
	}
	seen := make(map[string]bool, len(c.Databases))
	for i, db := range c.Databases {
		if db.Name == "" {
			return fmt.Errorf("databases[%d]: name must not be empty", i)
		}
		if db.Host == "" {
			return fmt.Errorf("databases[%d] %q: host must not be empty", i, db.Name)
		}
		if seen[db.Name] {
			return fmt.Errorf("databases[%d]: duplicate name %q", i, db.Name)
		}
		seen[db.Name] = true
	}
	return nil
}

// validatePostgresPort checks the port standalone mode connects with; fleet
// and meta-db modes take their connections from elsewhere.
func (c *Config) validatePostgresPort() error {
	if c.Mode != "standalone" || c.MetaDB != "" {
		return nil
	}
	if p := c.Postgres.Port; p < 1 || p > 65535 {
		return fmt.Errorf("postgres.port must be from 1 to 65535, got %d (standalone "+
			"mode connects with the postgres settings; --meta-db selects a meta-db fleet)", p)
	}
	return nil
}

// validateTrust checks the trust level and the maintenance window grammar
// so a typo fails at load instead of silently meaning "never".
func (c *Config) validateTrust() error {
	if !IsTrustLevel(c.Trust.Level) {
		return fmt.Errorf(
			"trust.level must be observation, advisory, or autonomous (got %q)",
			c.Trust.Level,
		)
	}
	if err := c.Trust.validateCeiling(); err != nil {
		return err
	}
	return ValidateMaintenanceWindow(c.Trust.MaintenanceWindow)
}

// validateMode rejects unknown modes, meta mode without a meta database,
// standalone mode with one (main would run the meta-db fleet while metrics
// report standalone), and the removed extension mode, which served the API
// over the deleted C extension's schema.
func validateMode(cfg *Config) error {
	switch cfg.Mode {
	case "standalone":
		if cfg.MetaDB != "" {
			return errors.New("mode standalone cannot be combined with " +
				"meta_db; drop mode (meta is inferred) or set mode: meta")
		}
		return nil
	case "fleet":
		return nil
	case ModeMeta:
		if cfg.MetaDB == "" {
			return fmt.Errorf("mode %q requires --meta-db (meta_db)", ModeMeta)
		}
		return nil
	case "extension":
		return fmt.Errorf("mode %q was removed with the C extension: use "+
			"standalone for one database, fleet for several, or --meta-db "+
			"for a meta-db fleet", cfg.Mode)
	default:
		return fmt.Errorf("invalid mode %q: must be 'standalone', 'fleet', "+
			"or 'meta'", cfg.Mode)
	}
}
