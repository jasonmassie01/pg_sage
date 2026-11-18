package config

import "fmt"

// Sage Review (review-01a): the deterministic, read-only grade of each
// database against the versioned rubric. On by default: it reads the
// producers pg_sage already runs and writes only sage.review_* rows.
const (
	DefaultReviewGradeIntervalHours = 24

	minReviewGradeIntervalHours = 1
	maxReviewGradeIntervalHours = 720
)

// ReviewConfig configures Sage Review. Both fields are read when a
// database's runtime starts (restart lifecycle).
type ReviewConfig struct {
	Enabled            bool `yaml:"enabled" doc:"Grade each database against the rubric after its first look, then every grade_interval_hours. Deterministic and read-only; writes only sage.review_* rows. Default: true."`
	GradeIntervalHours int  `yaml:"grade_interval_hours" doc:"Hours between two grades of one database. Range 1-720. Default: 24."`
}

func defaultReviewConfig() ReviewConfig {
	return ReviewConfig{Enabled: true, GradeIntervalHours: DefaultReviewGradeIntervalHours}
}

// validate checks the interval's range.
func (r ReviewConfig) validate() error {
	if r.GradeIntervalHours < minReviewGradeIntervalHours ||
		r.GradeIntervalHours > maxReviewGradeIntervalHours {
		return fmt.Errorf("review.grade_interval_hours must be %d-%d, got %d",
			minReviewGradeIntervalHours, maxReviewGradeIntervalHours, r.GradeIntervalHours)
	}
	return nil
}
