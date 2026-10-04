package config

import "fmt"

// OptimizerRejectionMemoryConfig controls how the index optimizer remembers
// HypoPG what-if rejections (llm.optimizer.rejection_memory). A candidate the
// model proposes again — under any name, with the INCLUDE list reordered,
// trimmed or extended — is not re-measured while the workload and table are
// materially unchanged, and the model is told which shapes were measured.
type OptimizerRejectionMemoryConfig struct {
	Enabled          bool    `yaml:"enabled" doc:"Remember HypoPG what-if rejections of LLM index candidates: skip re-measuring the same idea and tell the model it was measured. LLM candidates only. Default: true."`
	MaxAgeDays       int     `yaml:"max_age_days" doc:"Days a remembered rejection stays valid; after that the idea may be measured again. Range 1-90. Default: 7."`
	CallVolumeRatio  float64 `yaml:"call_volume_ratio" doc:"A target query whose call count grew or shrank by at least this factor since the rejection is a material workload change. Range >1-1000. Default: 2."`
	MeanTimeRatio    float64 `yaml:"mean_time_ratio" doc:"A target query whose mean execution time grew or shrank by at least this factor is a material workload change. Range >1-1000. Default: 2."`
	RowEstimateRatio float64 `yaml:"row_estimate_ratio" doc:"A table whose live-row estimate grew or shrank by at least this factor is a material change. Range >1-1000. Default: 2."`
	PromptMaxShapes  int     `yaml:"prompt_max_shapes" doc:"Most recently rejected shapes per table listed in the optimizer prompt as already measured. Range 1-20. Default: 5."`
	SkipLLMAfter     int     `yaml:"skip_llm_after" doc:"After this many consecutive proposals for a table that were all already measured or rejected, stop asking the model until a material change or the max age. Range 1-100. Default: 3."`
}

// Rejection memory defaults and limits.
const (
	DefaultOptRejectionMemoryEnabled    = true
	DefaultOptRejectionMaxAgeDays       = 7
	DefaultOptRejectionCallVolumeRatio  = 2.0
	DefaultOptRejectionMeanTimeRatio    = 2.0
	DefaultOptRejectionRowEstimateRatio = 2.0
	DefaultOptRejectionPromptMaxShapes  = 5
	DefaultOptRejectionSkipLLMAfter     = 3

	MaxOptRejectionMaxAgeDays      = 90
	MaxOptRejectionRatio           = 1000.0
	MaxOptRejectionPromptMaxShapes = 20
	MaxOptRejectionSkipLLMAfter    = 100
)

// DefaultOptimizerRejectionMemory returns the shipped rejection memory
// settings.
func DefaultOptimizerRejectionMemory() OptimizerRejectionMemoryConfig {
	return OptimizerRejectionMemoryConfig{
		Enabled:          DefaultOptRejectionMemoryEnabled,
		MaxAgeDays:       DefaultOptRejectionMaxAgeDays,
		CallVolumeRatio:  DefaultOptRejectionCallVolumeRatio,
		MeanTimeRatio:    DefaultOptRejectionMeanTimeRatio,
		RowEstimateRatio: DefaultOptRejectionRowEstimateRatio,
		PromptMaxShapes:  DefaultOptRejectionPromptMaxShapes,
		SkipLLMAfter:     DefaultOptRejectionSkipLLMAfter,
	}
}

const rejectionMemoryPrefix = "llm.optimizer.rejection_memory."

// validate refuses values outside their documented ranges. A ratio must be
// above 1 (a ratio of 1 would call every cycle a material change).
func (c OptimizerRejectionMemoryConfig) validate() error {
	if c.MaxAgeDays < 1 || c.MaxAgeDays > MaxOptRejectionMaxAgeDays {
		return fmt.Errorf("%smax_age_days must be 1-%d, got %d",
			rejectionMemoryPrefix, MaxOptRejectionMaxAgeDays, c.MaxAgeDays)
	}
	for _, r := range []struct {
		key   string
		value float64
	}{{"call_volume_ratio", c.CallVolumeRatio}, {"mean_time_ratio", c.MeanTimeRatio},
		{"row_estimate_ratio", c.RowEstimateRatio}} {
		if !(r.value > 1 && r.value <= MaxOptRejectionRatio) {
			return fmt.Errorf("%s%s must be greater than 1 and at most %g, got %g",
				rejectionMemoryPrefix, r.key, MaxOptRejectionRatio, r.value)
		}
	}
	if c.PromptMaxShapes < 1 || c.PromptMaxShapes > MaxOptRejectionPromptMaxShapes {
		return fmt.Errorf("%sprompt_max_shapes must be 1-%d, got %d",
			rejectionMemoryPrefix, MaxOptRejectionPromptMaxShapes, c.PromptMaxShapes)
	}
	if c.SkipLLMAfter < 1 || c.SkipLLMAfter > MaxOptRejectionSkipLLMAfter {
		return fmt.Errorf("%sskip_llm_after must be 1-%d, got %d",
			rejectionMemoryPrefix, MaxOptRejectionSkipLLMAfter, c.SkipLLMAfter)
	}
	return nil
}
