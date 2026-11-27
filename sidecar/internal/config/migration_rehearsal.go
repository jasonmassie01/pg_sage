package config

import (
	"fmt"
	"time"
)

// migration-06: budgets for a rehearsed apply_migration.
const (
	// DefaultMigrationContractDelayMinutes is
	// migration.contract_delay_minutes's default (1 hour).
	DefaultMigrationContractDelayMinutes = 60
	// DefaultMigrationMaxLockMS is migration.max_lock_ms's default.
	DefaultMigrationMaxLockMS = 2000
	// DefaultRehearsalPlanFlipPct is rehearsal.plan_flip_pct's default.
	DefaultRehearsalPlanFlipPct = 5.0
)

// RehearsalConfig tunes how a clone rehearsal judges the workload.
type RehearsalConfig struct {
	PlanFlipPct float64 `yaml:"plan_flip_pct" doc:"Percent a query may slow on the rehearsal clone after its plan changed before apply_migration parks (plan_regression); without a plan change the limit is 15. Above 0, at most 100. Default: 5."`
}

// validateMigrationRehearsal refuses values that would remove a budget:
// unlike most keys, zero here is not "use the default" but an error, so
// an operator never gets an unbounded budget by accident.
func (c *Config) validateMigrationRehearsal() error {
	if m := c.Migration.ContractDelayMinutes; m < 1 || m > MaxIntSetting {
		return fmt.Errorf("migration.contract_delay_minutes must be 1-%d, got %d",
			MaxIntSetting, m)
	}
	if c.Migration.MaxLockMS < 1 || c.Migration.MaxLockMS > MaxIntSetting {
		return fmt.Errorf("migration.max_lock_ms must be 1-%d, got %d",
			MaxIntSetting, c.Migration.MaxLockMS)
	}
	if pct := c.Rehearsal.PlanFlipPct; pct <= 0 || pct > 100 {
		return fmt.Errorf("rehearsal.plan_flip_pct must be above 0 and at most 100, got %v",
			pct)
	}
	return nil
}

// ContractDelay is migration.contract_delay_minutes as a duration.
func (m MigrationConfig) ContractDelay() time.Duration {
	return time.Duration(m.ContractDelayMinutes) * time.Minute
}

// MaxLock is migration.max_lock_ms as a duration.
func (m MigrationConfig) MaxLock() time.Duration {
	return time.Duration(m.MaxLockMS) * time.Millisecond
}
