package config

import (
	"fmt"
	"regexp"
	"strings"
)

// DefaultSubscriptionLagMinutes is how stale a subscription's last report to
// its publisher (pg_stat_subscription.latest_end_time) may be before the
// analyzer raises subscription_lag (market-13).
const DefaultSubscriptionLagMinutes = 5

// DefaultCDCStallMinutes is how long an active logical slot's
// confirmed_flush_lsn may stand still while WAL advances before the
// analyzer raises cdc_consumer_stalled (market-13).
const DefaultCDCStallMinutes = 10

// maxReplicationMinutes bounds the market-13 minute settings to one week.
const maxReplicationMinutes = 7 * 24 * 60

// ReplicationConfig is logical replication and CDC consumer health.
type ReplicationConfig struct {
	CDCHeartbeats []CDCHeartbeat `yaml:"cdc_heartbeats" doc:"Optional CDC heartbeat tables: [{slot, table, column, max_age_minutes}]. For a logical slot of the monitored database, pg_sage reads SELECT max(column) from table (schema.table or table, plain identifiers only) under a 2 s statement timeout and raises cdc_heartbeat_stale when the newest value is older than max_age_minutes. Empty runs nothing."`
}

// CDCHeartbeat is one heartbeat table a CDC connector writes through its
// slot (Debezium's heartbeat.action.query, for example).
type CDCHeartbeat struct {
	Slot          string `yaml:"slot"`
	Table         string `yaml:"table"`
	Column        string `yaml:"column"`
	MaxAgeMinutes int    `yaml:"max_age_minutes"`
}

// plainIdent is an unquoted SQL identifier: the only form a heartbeat table
// or column may take, so the name never needs more than double quotes.
var plainIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]*$`)

// validPlainTable reports whether table is "name" or "schema.name" of plain
// identifiers.
func validPlainTable(table string) bool {
	parts := strings.Split(table, ".")
	if len(parts) > 2 {
		return false
	}
	for _, p := range parts {
		if !plainIdent.MatchString(p) {
			return false
		}
	}
	return true
}

// validate refuses a heartbeat entry pg_sage could not read safely: a
// missing slot, a table or column that is not a plain identifier, a
// max_age_minutes outside 1-10080, or a second entry for one slot.
func (r ReplicationConfig) validate() error {
	seen := map[string]bool{}
	for i, hb := range r.CDCHeartbeats {
		at := fmt.Sprintf("replication.cdc_heartbeats[%d]", i)
		switch {
		case strings.TrimSpace(hb.Slot) == "":
			return fmt.Errorf("%s: slot is required", at)
		case !validPlainTable(hb.Table):
			return fmt.Errorf("%s: table %q must be table or schema.table of plain "+
				"identifiers (letters, digits, _ and $)", at, hb.Table)
		case !plainIdent.MatchString(hb.Column):
			return fmt.Errorf("%s: column %q must be a plain identifier "+
				"(letters, digits, _ and $)", at, hb.Column)
		case hb.MaxAgeMinutes < 1 || hb.MaxAgeMinutes > maxReplicationMinutes:
			return fmt.Errorf("%s: max_age_minutes must be 1-%d, got %d", at,
				maxReplicationMinutes, hb.MaxAgeMinutes)
		case seen[hb.Slot]:
			return fmt.Errorf("%s: slot %q already has a heartbeat", at, hb.Slot)
		}
		seen[hb.Slot] = true
	}
	return nil
}

// validateReplicationMinutes refuses a market-13 analyzer threshold outside
// 0-10080 (0 uses the default).
func (a AnalyzerConfig) validateReplicationMinutes() error {
	for _, k := range []struct {
		key string
		v   int
	}{
		{"analyzer.subscription_lag_minutes", a.SubscriptionLagMinutes},
		{"analyzer.cdc_stall_minutes", a.CDCStallMinutes},
	} {
		if k.v < 0 || k.v > maxReplicationMinutes {
			return fmt.Errorf("%s must be 0-%d, got %d", k.key, maxReplicationMinutes, k.v)
		}
	}
	return nil
}
