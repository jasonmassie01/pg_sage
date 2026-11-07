package config

import (
	"fmt"
	"time"
)

// rca-01: the vacuum_blocked tree names an xmin holder only once its
// transaction has been open rca.xmin_holder_min_minutes (every open
// transaction holds a virtualxid lock, so a 50 ms idle session was
// named), and a prepared transaction only once it is
// rca.prepared_xact_min_age_minutes old (two-phase commit prepares and
// commits many in normal operation).
const (
	DefaultRCAXminHolderMinMinutes      = 5
	DefaultRCAPreparedXactMinAgeMinutes = 30
	maxRCAXminHolderMinMinutes          = 1440  // a day
	maxRCAPreparedXactMinAgeMinutes     = 10080 // a week
)

// XminHolderFloor is how long a session's transaction must be open before
// it is named as the xmin holder (zero key: the default).
func (r *RCAConfig) XminHolderFloor() time.Duration {
	m := r.XminHolderMinMinutes
	if m <= 0 {
		m = DefaultRCAXminHolderMinMinutes
	}
	return time.Duration(m) * time.Minute
}

// PreparedXactFloor is how old a prepared transaction must be before it
// is signalled or blamed (zero key: the default).
func (r *RCAConfig) PreparedXactFloor() time.Duration {
	m := r.PreparedXactMinAgeMinutes
	if m <= 0 {
		m = DefaultRCAPreparedXactMinAgeMinutes
	}
	return time.Duration(m) * time.Minute
}

func (r *RCAConfig) validateHolderFloor() error {
	if m := r.XminHolderMinMinutes; m < 1 || m > maxRCAXminHolderMinMinutes {
		return fmt.Errorf("rca.xmin_holder_min_minutes must be 1-%d, got %d",
			maxRCAXminHolderMinMinutes, m)
	}
	if m := r.PreparedXactMinAgeMinutes; m < 1 || m > maxRCAPreparedXactMinAgeMinutes {
		return fmt.Errorf("rca.prepared_xact_min_age_minutes must be 1-%d, got %d",
			maxRCAPreparedXactMinAgeMinutes, m)
	}
	return nil
}
