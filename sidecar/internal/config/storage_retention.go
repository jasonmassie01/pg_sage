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

// validateStorage refuses query_store_days outside 0-3650 and
// snapshots_max_pct outside 0-100 (0 disables either).
func (r RetentionConfig) validateStorage() error {
	if r.QueryStoreDays < 0 || r.QueryStoreDays > maxRetentionQueryStoreDays {
		return fmt.Errorf("retention.query_store_days must be 0-%d, got %d",
			maxRetentionQueryStoreDays, r.QueryStoreDays)
	}
	if r.SnapshotsMaxPct < 0 || r.SnapshotsMaxPct > 100 {
		return fmt.Errorf("retention.snapshots_max_pct must be 0-100, got %d",
			r.SnapshotsMaxPct)
	}
	return nil
}
