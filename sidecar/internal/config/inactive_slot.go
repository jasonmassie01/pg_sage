package config

import "fmt"

// DefaultInactiveSlotMinMinutes is how long a replication slot must stay
// inactive before the analyzer reports it: long enough to ride out a CDC
// connector's reconnect or a nightly consumer's restart.
const DefaultInactiveSlotMinMinutes = 5

// MaxInactiveSlotMinMinutes is one week, the largest threshold YAML and
// the configuration API accept.
const MaxInactiveSlotMinMinutes = 7 * 24 * 60

// validateInactiveSlotMinMinutes refuses a threshold outside 0-10080
// (0 uses the default).
func (a AnalyzerConfig) validateInactiveSlotMinMinutes() error {
	if a.InactiveSlotMinMinutes < 0 || a.InactiveSlotMinMinutes > MaxInactiveSlotMinMinutes {
		return fmt.Errorf("analyzer.inactive_slot_min_minutes must be 0-%d, got %d",
			MaxInactiveSlotMinMinutes, a.InactiveSlotMinMinutes)
	}
	return nil
}
