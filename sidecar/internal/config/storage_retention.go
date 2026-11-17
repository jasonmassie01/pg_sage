package config

import "fmt"

// DefaultRetentionQueryStoreDays keeps two weeks of per-query samples.
// Every reader of sage.query_store looks back at most 7 days (verification
// windows, the SLO latency proxy, plan regressions); at 90 days (the
// snapshots window it used to follow) lifeos projected 35M rows, 14 GB.
const DefaultRetentionQueryStoreDays = 14

// DefaultRetentionSnapshotsMaxPct caps sage.snapshots at 5% of the
// database: half of DefaultRetentionSageSizeWarningPct, so snapshots alone
// never raise the sage_footprint finding.
const DefaultRetentionSnapshotsMaxPct = 5

// MinSnapshotCapBytes is the smallest snapshot size cap: on a small
// database a percentage would leave too little history to forecast from.
// The sage_footprint finding uses the same floor: below it, pg_sage's own
// data is no finding however small the database.
const MinSnapshotCapBytes int64 = 256 << 20

const maxRetentionQueryStoreDays = 3650

// DefaultRetentionQueryRollupHourlyDays keeps the hourly per-query rollup
// 35 days: two weeks for every hour-of-week bucket twice plus the
// baseline's minimum weeks (R6 section 6). DefaultRetentionQueryRollupDailyDays
// keeps the daily tier and the hourly coverage 400 days, so a long window
// can still state its coverage after the hourly rows are gone (evid-01b).
const (
	DefaultRetentionQueryRollupHourlyDays = 35
	DefaultRetentionQueryRollupDailyDays  = 400
)

// validateStorage refuses query_store_days outside 0-3650 and
// snapshots_max_pct outside 0-100 (0 disables either), and the rollup
// windows (validateRollup).
func (r RetentionConfig) validateStorage() error {
	if r.QueryStoreDays < 0 || r.QueryStoreDays > maxRetentionQueryStoreDays {
		return fmt.Errorf("retention.query_store_days must be 0-%d, got %d",
			maxRetentionQueryStoreDays, r.QueryStoreDays)
	}
	if r.SnapshotsMaxPct < 0 || r.SnapshotsMaxPct > 100 {
		return fmt.Errorf("retention.snapshots_max_pct must be 0-100, got %d",
			r.SnapshotsMaxPct)
	}
	return r.validateRollup()
}

// validateRollup refuses rollup windows outside 0-3650 (0 disables), an
// hourly window longer than the snapshots it is rebuilt from (the default
// follows a smaller snapshots_days instead), and a daily window shorter
// than the hourly one (an hour is purged only once its day is rolled).
func (r RetentionConfig) validateRollup() error {
	for _, k := range []struct {
		key  string
		days int
	}{
		{"query_rollup_hourly_days", r.QueryRollupHourlyDays},
		{"query_rollup_daily_days", r.QueryRollupDailyDays},
	} {
		if k.days < 0 || k.days > maxRetentionQueryStoreDays {
			return fmt.Errorf("retention.%s must be 0-%d, got %d", k.key,
				maxRetentionQueryStoreDays, k.days)
		}
	}
	h := r.QueryRollupHourlyDays
	if h != DefaultRetentionQueryRollupHourlyDays && h > 0 && r.SnapshotsDays > 0 &&
		h > r.SnapshotsDays {
		return fmt.Errorf("retention.query_rollup_hourly_days (%d) cannot exceed "+
			"retention.snapshots_days (%d): the hourly rollup is rebuilt from the "+
			"snapshots", h, r.SnapshotsDays)
	}
	if d := r.QueryRollupDailyDays; d > 0 && h > 0 && d < h {
		return fmt.Errorf("retention.query_rollup_daily_days (%d) must be at least "+
			"retention.query_rollup_hourly_days (%d): an hourly row is removed only "+
			"once its day is in the daily tier", d, h)
	}
	return nil
}

// EffectiveQueryRollupHourlyDays is the hourly rollup's retention: the
// configured days, never past a smaller (non-zero) snapshots_days.
func (r RetentionConfig) EffectiveQueryRollupHourlyDays() int {
	h := r.QueryRollupHourlyDays
	if h > 0 && r.SnapshotsDays > 0 && r.SnapshotsDays < h {
		return r.SnapshotsDays
	}
	return h
}
