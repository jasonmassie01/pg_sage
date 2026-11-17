package config

import "fmt"

// Sage Review (review-01a): the deterministic, read-only grade of each
// database against the versioned rubric. On by default: it reads the
// producers pg_sage already runs and writes only sage.review_* rows.
const (
	DefaultReviewGradeIntervalHours = 24
	// DefaultReviewWindowDays is the generators' window (review-02a).
	DefaultReviewWindowDays = 90

	minReviewGradeIntervalHours = 1
	maxReviewGradeIntervalHours = 720
	// The window's bounds are the workload pack's (workpack.Window).
	minReviewWindowDays = 14
	maxReviewWindowDays = 100
)

// ReviewConfig configures Sage Review. Every field is read when a
// database's runtime starts (restart lifecycle).
type ReviewConfig struct {
	Enabled            bool `yaml:"enabled" doc:"Grade each database against the rubric after its first look, then every grade_interval_hours. Deterministic and read-only; writes only sage.review_* rows. Default: true."`
	GradeIntervalHours int  `yaml:"grade_interval_hours" doc:"Hours between two grades of one database. Range 1-720. Default: 24."`
	WindowDays         int  `yaml:"window_days" doc:"Days of history the review generators judge (index portfolio, ...); a pack shorter than this, as retention allows, is partial coverage. Range 14-100. Default: 90."`
}

func defaultReviewConfig() ReviewConfig {
	return ReviewConfig{Enabled: true, GradeIntervalHours: DefaultReviewGradeIntervalHours,
		WindowDays: DefaultReviewWindowDays}
}

// validate checks the interval's and the window's ranges.
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
	return nil
}
