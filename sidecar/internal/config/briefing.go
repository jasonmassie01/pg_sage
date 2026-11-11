package config

import "fmt"

// BriefingOwner is the reconfiguration owner of briefing.schedule,
// briefing.channels and briefing.slack_webhook_url (llm-06): a live change
// swaps every briefing worker's settings in place.
const BriefingOwner = "briefing"

// briefing.fleet_mode values. Empty means rollup.
const (
	BriefingFleetRollup      = "rollup"
	BriefingFleetPerDatabase = "per_database"
)

// validate checks briefing.fleet_mode. The schedule is checked by the
// briefing owner at apply time, where the cron parser lives.
func (b BriefingConfig) validate() error {
	switch b.FleetMode {
	case "", BriefingFleetRollup, BriefingFleetPerDatabase:
		return nil
	}
	return fmt.Errorf("briefing.fleet_mode %q: want %q or %q",
		b.FleetMode, BriefingFleetRollup, BriefingFleetPerDatabase)
}

// RollsUpBriefings reports whether one process-level message covers every
// fleet database: a YAML fleet or a meta-db fleet (mode meta, or fleet with
// meta_db) with fleet_mode rollup (the default).
func (c *Config) RollsUpBriefings() bool {
	return c != nil && (c.IsFleet() || c.HasMetaDB()) &&
		c.Briefing.FleetMode != BriefingFleetPerDatabase
}
