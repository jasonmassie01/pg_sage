package config

import "fmt"

// Sage Review (review-01a): the deterministic, read-only grade of each
// database against the versioned rubric. On by default: it reads the
// producers pg_sage already runs and writes only sage.review_* rows.
const (
	DefaultReviewGradeIntervalHours = 24
	// DefaultReviewWindowDays is the generators' window (review-02a).
	DefaultReviewWindowDays = 90
	// DefaultReviewLargeTableBytes is the schema-debt generator's giant
	// table size (review-02d): 10 GiB.
	DefaultReviewLargeTableBytes int64 = 10 << 30

	minReviewGradeIntervalHours = 1
	maxReviewGradeIntervalHours = 720
	// The window's bounds are the workload pack's (workpack.Window).
	minReviewWindowDays            = 14
	maxReviewWindowDays            = 100
	minReviewLargeTableBytes int64 = 1 << 30
	maxReviewLargeTableBytes int64 = 1 << 50
)

// ReviewConfig configures Sage Review. Every field is read when a
// database's runtime starts (restart lifecycle).
type ReviewConfig struct {
	Enabled            bool  `yaml:"enabled" doc:"Grade each database against the rubric after its first look, then every grade_interval_hours. Deterministic and read-only; writes only sage.review_* rows. Default: true."`
	GradeIntervalHours int   `yaml:"grade_interval_hours" doc:"Hours between two grades of one database. Range 1-720. Default: 24."`
	WindowDays         int   `yaml:"window_days" doc:"Days of history the review generators judge (index portfolio, ...); a pack shorter than this, as retention allows, is partial coverage. Range 14-100. Default: 90."`
	LargeTableBytes    int64 `yaml:"large_table_bytes" doc:"Bytes from which Sage Review advises partitioning an unpartitioned table with a time-ordered key. Range 1073741824-1125899906842624 (1 GiB-1 PiB). Default: 10737418240 (10 GiB)."`
}

func defaultReviewConfig() ReviewConfig {
	return ReviewConfig{Enabled: true, GradeIntervalHours: DefaultReviewGradeIntervalHours,
		WindowDays: DefaultReviewWindowDays, LargeTableBytes: DefaultReviewLargeTableBytes}
}

// validate checks the interval's, the window's and the table size's
// ranges.
func (r ReviewConfig) validate() error {
	if r.GradeIntervalHours < minReviewGradeIntervalHours ||
		r.GradeIntervalHours > maxReviewGradeIntervalHours {
		return fmt.Errorf("review.grade_interval_hours must be %d-%d, got %d",
			minReviewGradeIntervalHours, maxReviewGradeIntervalHours, r.GradeIntervalHours)
	}
	if r.WindowDays < minReviewWindowDays || r.WindowDays > maxReviewWindowDays {
		return fmt.Errorf("review.window_days must be %d-%d, got %d", minReviewWindowDays,
			maxReviewWindowDays, r.WindowDays)
	}
	if r.LargeTableBytes < minReviewLargeTableBytes ||
		r.LargeTableBytes > maxReviewLargeTableBytes {
		return fmt.Errorf("review.large_table_bytes must be %d-%d, got %d",
			minReviewLargeTableBytes, maxReviewLargeTableBytes, r.LargeTableBytes)
	}
	return nil
}
