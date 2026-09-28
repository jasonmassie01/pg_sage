package config

import "fmt"

// validateIOAdmission checks the D6 load-admission keys. A declared
// capacity loosens a safety gate, so it must be complete and positive, and
// in fleet mode it must be made per database.
func (c *Config) validateIOAdmission() error {
	if c.Safety.DataIOCeilingPct <= 0 || c.Safety.DataIOCeilingPct > 100 {
		return fmt.Errorf("safety.data_io_ceiling_pct must be 1-100")
	}
	if c.Safety.WALIOCeilingPct <= 0 || c.Safety.WALIOCeilingPct > 100 {
		return fmt.Errorf("safety.wal_io_ceiling_pct must be 1-100")
	}
	if c.Verify.IOBaselineDays < 0 {
		return fmt.Errorf("verify.io_baseline_days must be 0 (disabled) or positive")
	}
	if c.Verify.IOSampleDays <= 0 ||
		c.Verify.IOSampleDays < c.Verify.IOBaselineDays {
		return fmt.Errorf(
			"verify.io_sample_retention_days must be positive and at least io_baseline_days")
	}
	if c.Verify.IOCapacity != nil && c.Mode != "" && c.Mode != "standalone" {
		return fmt.Errorf("verify.io_capacity is standalone only; " +
			"declare capacity per database with databases[].verify.io_capacity")
	}
	if err := c.Verify.IOCapacity.validate("verify.io_capacity"); err != nil {
		return err
	}
	for i, db := range c.Databases {
		field := fmt.Sprintf("databases[%d] %q: verify.io_capacity", i, db.Name)
		if err := db.Verify.IOCapacity.validate(field); err != nil {
			return err
		}
	}
	return nil
}

func (capacity *IOCapacityConfig) validate(field string) error {
	if capacity == nil {
		return nil
	}
	if capacity.ReadWriteMBps <= 0 || capacity.WALMBps <= 0 {
		return fmt.Errorf("%s: read_write_mbps and wal_mbps must both be positive", field)
	}
	return nil
}

func cloneIOCapacity(capacity *IOCapacityConfig) *IOCapacityConfig {
	if capacity == nil {
		return nil
	}
	cloned := *capacity
	return &cloned
}
