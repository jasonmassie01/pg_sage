package config

import "fmt"

// DefaultRetentionSageSizeWarningPct is the share of the database (in
// percent) pg_sage's own tables may take before the analyzer raises a
// sage_footprint finding. pg_sage guards the database; it must not become
// a large part of it (dogfood lifeos: 9.3 GB of snapshots).
const DefaultRetentionSageSizeWarningPct = 10

// validateSageFootprint refuses a share outside 0-100 (0 disables).
func (r RetentionConfig) validateSageFootprint() error {
	if r.SageSizeWarningPct < 0 || r.SageSizeWarningPct > 100 {
		return fmt.Errorf("retention.sage_size_warning_pct must be 0-100, got %d",
			r.SageSizeWarningPct)
	}
	return nil
}
