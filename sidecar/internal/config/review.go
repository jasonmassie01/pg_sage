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

// The model review pass (review-03a).
const (
	// ReviewModelPassAuto runs the pass when any model is usable;
	// ReviewModelPassOn always tries it; ReviewModelPassOff never does.
	ReviewModelPassAuto = "auto"
	ReviewModelPassOn   = "on"
	ReviewModelPassOff  = "off"
	// ReviewConsensusMajority keeps what most runs keep;
	// ReviewConsensusRefute has a second run attack the first run's picks.
	ReviewConsensusMajority = "majority"
	ReviewConsensusRefute   = "refute"
	// DefaultReviewModelRuns is how many independent runs vote.
	DefaultReviewModelRuns = 3
	maxReviewModelRuns     = 5
)

// ReviewConfig configures Sage Review. Every field is read when a
// database's runtime starts (restart lifecycle).
type ReviewConfig struct {
	Enabled            bool   `yaml:"enabled" doc:"Grade each database against the rubric after its first look, then every grade_interval_hours. Deterministic and read-only; writes only sage.review_* rows. Default: true."`
	GradeIntervalHours int    `yaml:"grade_interval_hours" doc:"Hours between two grades of one database. Range 1-720. Default: 24."`
	WindowDays         int    `yaml:"window_days" doc:"Days of history the review generators judge (index portfolio, ...); a pack shorter than this, as retention allows, is partial coverage. Range 14-100. Default: 90."`
	LargeTableBytes    int64  `yaml:"large_table_bytes" doc:"Bytes from which Sage Review advises partitioning an unpartitioned table with a time-ordered key. Range 1073741824-1125899906842624 (1 GiB-1 PiB). Default: 10737418240 (10 GiB)."`
	ModelPass          string `yaml:"model_pass" doc:"Model pass that ranks, attacks and links the review's candidates: auto (when a model is usable), on (no model is a failed pass) or off (deterministic ranking only). Default: auto."`
	ModelRuns          int    `yaml:"model_runs" doc:"Independent model runs per review whose results are combined by review.consensus. An odd number in 1-5; 1 applies no consensus. Default: 3."`
	Consensus          string `yaml:"consensus" doc:"How model runs combine: majority (keep what most runs keep, at the median rank) or refute (a second run must refute each kept item). Default: majority."`
	HashIdentifiers    bool   `yaml:"hash_identifiers" doc:"Replace schema, table, index and column names with keyed pseudonyms in what the review model reads (needs encryption_key). Default: false."`
	// Gate is the model gate's thresholds (review-05a).
	Gate ReviewGateConfig `yaml:"gate"`
}

func defaultReviewConfig() ReviewConfig {
	return ReviewConfig{Enabled: true, GradeIntervalHours: DefaultReviewGradeIntervalHours,
		WindowDays: DefaultReviewWindowDays, LargeTableBytes: DefaultReviewLargeTableBytes,
		ModelPass: ReviewModelPassAuto, ModelRuns: DefaultReviewModelRuns,
		Consensus: ReviewConsensusMajority, Gate: defaultReviewGateConfig()}
}

// validate checks the interval's, the window's and the table size's
// ranges, then the model pass's words and run count.
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
	if err := r.validateModelPass(); err != nil {
		return err
	}
	return r.Gate.validate()
}

func (r ReviewConfig) validateModelPass() error {
	switch r.ModelPass {
	case ReviewModelPassAuto, ReviewModelPassOn, ReviewModelPassOff:
	default:
		return fmt.Errorf("review.model_pass must be auto, on or off, got %q", r.ModelPass)
	}
	if r.ModelRuns < 1 || r.ModelRuns > maxReviewModelRuns || r.ModelRuns%2 == 0 {
		return fmt.Errorf("review.model_runs must be odd, 1-%d, got %d", maxReviewModelRuns,
			r.ModelRuns)
	}
	switch r.Consensus {
	case ReviewConsensusMajority, ReviewConsensusRefute:
	default:
		return fmt.Errorf("review.consensus must be majority or refute, got %q", r.Consensus)
	}
	return nil
}
