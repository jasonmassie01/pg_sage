package config

import (
	"strings"
	"testing"
)

// evid-01b: the per-query rollup keeps its hourly tier
// retention.query_rollup_hourly_days (default 35) and its daily tier and
// hourly coverage retention.query_rollup_daily_days (default 400). Both are
// 0-3650 (0 disables); the hourly tier cannot outlive the snapshots it is
// rebuilt from, and the daily tier cannot be shorter than the hourly one.
// No concurrent access tests: validation is a pure function of the values.

func TestRollupRetention_DefaultsWithoutConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r := cfg.Retention
	if r.QueryRollupHourlyDays != 35 || DefaultRetentionQueryRollupHourlyDays != 35 {
		t.Fatalf("query_rollup_hourly_days = %d (const %d), want 35",
			r.QueryRollupHourlyDays, DefaultRetentionQueryRollupHourlyDays)
	}
	if r.QueryRollupDailyDays != 400 || DefaultRetentionQueryRollupDailyDays != 400 {
		t.Fatalf("query_rollup_daily_days = %d (const %d), want 400",
			r.QueryRollupDailyDays, DefaultRetentionQueryRollupDailyDays)
	}
	if d := DefaultConfig().Retention; d.QueryRollupHourlyDays != 35 ||
		d.QueryRollupDailyDays != 400 {
		t.Fatalf("DefaultConfig disagrees with Load(nil): %+v", d)
	}
	if got := r.EffectiveQueryRollupHourlyDays(); got != 35 {
		t.Fatalf("effective hourly days = %d with snapshots_days 90, want 35", got)
	}
}

func TestRollupRetention_OutOfRangeNamesTheKey(t *testing.T) {
	for _, tc := range []struct {
		yaml, key string
	}{
		{"query_rollup_daily_days: -1", "retention.query_rollup_daily_days"},
		{"query_rollup_daily_days: 3651", "retention.query_rollup_daily_days"},
		{"query_rollup_hourly_days: -1", "retention.query_rollup_hourly_days"},
		{"query_rollup_hourly_days: 3651", "retention.query_rollup_hourly_days"},
	} {
		_, err := loadRCAYAML(t, "retention:\n  snapshots_days: 0\n  "+tc.yaml+"\n")
		if err == nil || !strings.Contains(err.Error(), tc.key) ||
			!strings.Contains(err.Error(), "0-3650") {
			t.Fatalf("%s: err = %v, want one naming %s and its range", tc.yaml, err, tc.key)
		}
	}
}

func TestRollupRetention_Boundaries(t *testing.T) {
	for _, tc := range []struct {
		body string
		ok   bool
		want string
	}{
		{"query_rollup_daily_days: 0", true, ""},
		{"query_rollup_daily_days: 3650", true, ""},
		{"query_rollup_hourly_days: 0", true, ""},
		{"snapshots_days: 40\n  query_rollup_hourly_days: 40", true, ""},
		{"snapshots_days: 40\n  query_rollup_hourly_days: 41", false,
			"cannot exceed retention.snapshots_days"},
		{"snapshots_days: 0\n  query_rollup_hourly_days: 3650\n  query_rollup_daily_days: 0",
			true, ""},
		{"query_rollup_hourly_days: 30\n  query_rollup_daily_days: 29", false,
			"retention.query_rollup_daily_days"},
		{"query_rollup_hourly_days: 30\n  query_rollup_daily_days: 30", true, ""},
	} {
		_, err := loadRCAYAML(t, "retention:\n  "+tc.body+"\n")
		if tc.ok && err != nil {
			t.Fatalf("%q must load: %v", tc.body, err)
		}
		if !tc.ok && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Fatalf("%q: err = %v, want one containing %q", tc.body, err, tc.want)
		}
	}
}

// A config that set only snapshots_days below the hourly default (valid
// before evid-01b) must still load: the default follows the snapshots.
func TestRollupRetention_DefaultFollowsASmallerSnapshotWindow(t *testing.T) {
	cfg, err := loadRCAYAML(t, "retention:\n  snapshots_days: 30\n")
	if err != nil {
		t.Fatalf("snapshots_days 30 alone must load: %v", err)
	}
	r := cfg.Retention
	if r.QueryRollupHourlyDays != 35 || r.EffectiveQueryRollupHourlyDays() != 30 {
		t.Fatalf("hourly days %d, effective %d: want the default 35 kept and 30 effective",
			r.QueryRollupHourlyDays, r.EffectiveQueryRollupHourlyDays())
	}
	r.SnapshotsDays = 0
	if got := r.EffectiveQueryRollupHourlyDays(); got != 35 {
		t.Fatalf("snapshots kept forever: effective hourly days %d, want 35", got)
	}
	var zero RetentionConfig
	if got := zero.EffectiveQueryRollupHourlyDays(); got != 0 {
		t.Fatalf("a zero (literal) config disables the purge, got %d", got)
	}
}
