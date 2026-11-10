package config

import "fmt"

// Binding facts (roadmap 2.3, facts-08). A confirmed operator or model fact
// keeps binding while its subject is absent for up to confirmed_absence_days
// (a table rebuilt and swapped in, a slot recreated after a connector
// reset); after that it expires and operators are notified. If the same
// subject comes back with the same value within as many days of the
// expiry, the confirmation is restored. Detector proposals and confirmed
// detector facts keep their one-hour absence grace.
const (
	DefaultFactsConfirmedAbsenceDays = 7

	minFactsConfirmedAbsenceDays = 1
	maxFactsConfirmedAbsenceDays = 90
)

// FactsConfig configures binding facts. Read when a database's runtime
// starts (restart lifecycle); one setting for every database of the fleet.
type FactsConfig struct {
	ConfirmedAbsenceDays int `yaml:"confirmed_absence_days" doc:"Days a confirmed operator or model fact keeps binding while its subject is absent, and may be restored after. Range 1-90. Default: 7."`
}

func defaultFactsConfig() FactsConfig {
	return FactsConfig{ConfirmedAbsenceDays: DefaultFactsConfirmedAbsenceDays}
}

func (f FactsConfig) validate() error {
	if f.ConfirmedAbsenceDays < minFactsConfirmedAbsenceDays ||
		f.ConfirmedAbsenceDays > maxFactsConfirmedAbsenceDays {
		return fmt.Errorf("facts.confirmed_absence_days must be %d-%d, got %d",
			minFactsConfirmedAbsenceDays, maxFactsConfirmedAbsenceDays,
			f.ConfirmedAbsenceDays)
	}
	return nil
}
