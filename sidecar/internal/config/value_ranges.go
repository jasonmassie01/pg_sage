package config

import (
	"fmt"
	"strconv"
	"strings"
)

// MaxIntSetting is the ceiling the override API applies to every integer
// setting; the YAML range checks share it so both paths reject the same
// values (config-02).
const MaxIntSetting = 1 << 30

// ParseBool is the one boolean grammar for string-valued settings: the
// override API's validator, every hot-reload setter and the read model use
// it, so a value that validates is the value that applies. It is
// strconv.ParseBool on lowercased input: true, t, 1, false, f and 0 in any
// case.
func ParseBool(value string) (bool, error) {
	return strconv.ParseBool(strings.ToLower(value))
}

type intRange struct {
	key      string
	value    int
	min, max int
}

// apiTypedRanges mirrors the override API's types (store.allowedConfigKeys)
// for settings whose zero or negative value is harmful: an unused-index
// window of 0 reports every index unused, a DDL timeout of 0 means no
// statement_timeout. Keys documented with "0 disables" are not listed.
func (c *Config) apiTypedRanges() []intRange {
	const minFive = 5 // the API's int_min5 interval floor
	return []intRange{
		{"analyzer.unused_index_window_days", c.Analyzer.UnusedIndexWindowDays,
			1, MaxIntSetting},
		{"analyzer.seq_scan_min_rows", c.Analyzer.SeqScanMinRows, 1, MaxIntSetting},
		{"safety.ddl_timeout_seconds", c.Safety.DDLTimeoutSeconds, 1, MaxIntSetting},
		{"trust.rollback_threshold_pct", c.Trust.RollbackThresholdPct, 0, 100},
		{"trust.rollback_window_minutes", c.Trust.RollbackWindowMinutes,
			1, MaxIntSetting},
		{"trust.rollback_cooldown_days", c.Trust.RollbackCooldownDays,
			1, MaxIntSetting},
		{"advisor.interval_seconds", c.Advisor.IntervalSeconds, minFive, MaxIntSetting},
		{"alerting.check_interval_seconds", c.Alerting.CheckIntervalSeconds,
			minFive, MaxIntSetting},
		{"schema_lint.scan_interval_minutes", c.SchemaLint.ScanIntervalMinutes,
			1, MaxIntSetting},
		{"migration.poll_interval_seconds", c.Migration.PollIntervalSeconds,
			1, MaxIntSetting},
	}
}

// validateAPITypedRanges rejects, naming the key, any value the override
// API would refuse to save.
func (c *Config) validateAPITypedRanges() error {
	for _, r := range c.apiTypedRanges() {
		if r.value < r.min || r.value > r.max {
			return fmt.Errorf("%s must be %d-%d, got %d", r.key, r.min, r.max, r.value)
		}
	}
	return nil
}
