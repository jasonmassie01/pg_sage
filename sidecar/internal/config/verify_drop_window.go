package config

import (
	"fmt"
	"time"
)

// DefaultVerifyDropWindowHours is the business cycle an index drop is
// verified over (Phase 1.3): a week, so weekly jobs that need the index
// run inside the window. Shorter is a fast-elevation setting.
const DefaultVerifyDropWindowHours = 168

// DropWindow is verify.drop_window_hours as a duration.
func (v VerifyConfig) DropWindow() time.Duration {
	return time.Duration(v.DropWindowHours) * time.Hour
}

// validateDropWindow refuses a drop window outside 1-8760 hours: no value
// means "skip verification".
func (v VerifyConfig) validateDropWindow() error {
	if v.DropWindowHours < 1 || v.DropWindowHours > maxElevationHours {
		return fmt.Errorf("verify.drop_window_hours must be 1-%d, got %d",
			maxElevationHours, v.DropWindowHours)
	}
	return nil
}
