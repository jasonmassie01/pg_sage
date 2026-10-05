package config

import "fmt"

// TuningConfig controls the case-driven tuning agent (roadmap 2.2): one
// agent per database that replaces the optimizer, advisor and tuner
// prompts. It asks the model only about workload cases (top statements,
// regressions, write amplification), within a per-database budget per
// analyzer cycle, and ranks its typed proposals by a confidence calibrated
// on the outcome ledger. Which proposal types it may make follows the
// existing switches (llm.optimizer.enabled, advisor.memory_enabled,
// advisor.vacuum_enabled, tuner.llm_enabled).
type TuningConfig struct {
	Enabled                bool `yaml:"enabled" doc:"Run the case-driven tuning agent when an LLM is usable. Its changes execute only through the policy gate and trust level. Default: true."`
	MaxCasesPerCycle       int  `yaml:"max_cases_per_cycle" doc:"Workload cases the agent may examine with the model per analyzer cycle; the rest wait. Range 1-20. Default: 3."`
	MaxRequestsPerCycle    int  `yaml:"max_requests_per_cycle" doc:"Model requests (tool turns included) per database per analyzer cycle. Range 1-200. Default: 12."`
	MaxTokensPerCycle      int  `yaml:"max_tokens_per_cycle" doc:"Model tokens per database per analyzer cycle, charged before each request. Range 1000-2000000. Default: 60000."`
	MaxTurnsPerCase        int  `yaml:"max_turns_per_case" doc:"Model turns per case; the last turn must answer without tools. Range 1-20. Default: 6."`
	MaxProposalsPerCycle   int  `yaml:"max_proposals_per_cycle" doc:"New findings the agent emits per cycle, best ranked first. Range 1-100. Default: 10."`
	CalibrationMinOutcomes int  `yaml:"calibration_min_outcomes" doc:"Decided outcomes of an action class and prediction method needed before a confidence is shown; below it the agent reports uncalibrated. Range 1-1000. Default: 5."`
	CalibrationWindowDays  int  `yaml:"calibration_window_days" doc:"Days of decided outcomes the calibration reads. Range 1-3650. Default: 180."`
}

// Tuning agent defaults.
const (
	DefaultTuningEnabled                = true
	DefaultTuningMaxCasesPerCycle       = 3
	DefaultTuningMaxRequestsPerCycle    = 12
	DefaultTuningMaxTokensPerCycle      = 60000
	DefaultTuningMaxTurnsPerCase        = 6
	DefaultTuningMaxProposalsPerCycle   = 10
	DefaultTuningCalibrationMinOutcomes = 5
	DefaultTuningCalibrationWindowDays  = 180
)

// DefaultTuning returns the shipped tuning agent settings.
func DefaultTuning() TuningConfig {
	return TuningConfig{
		Enabled:                DefaultTuningEnabled,
		MaxCasesPerCycle:       DefaultTuningMaxCasesPerCycle,
		MaxRequestsPerCycle:    DefaultTuningMaxRequestsPerCycle,
		MaxTokensPerCycle:      DefaultTuningMaxTokensPerCycle,
		MaxTurnsPerCase:        DefaultTuningMaxTurnsPerCase,
		MaxProposalsPerCycle:   DefaultTuningMaxProposalsPerCycle,
		CalibrationMinOutcomes: DefaultTuningCalibrationMinOutcomes,
		CalibrationWindowDays:  DefaultTuningCalibrationWindowDays,
	}
}

// validate refuses values outside their documented ranges.
func (c TuningConfig) validate() error {
	for _, r := range []struct {
		key           string
		value, lo, hi int
	}{
		{"max_cases_per_cycle", c.MaxCasesPerCycle, 1, 20},
		{"max_requests_per_cycle", c.MaxRequestsPerCycle, 1, 200},
		{"max_tokens_per_cycle", c.MaxTokensPerCycle, 1000, 2000000},
		{"max_turns_per_case", c.MaxTurnsPerCase, 1, 20},
		{"max_proposals_per_cycle", c.MaxProposalsPerCycle, 1, 100},
		{"calibration_min_outcomes", c.CalibrationMinOutcomes, 1, 1000},
		{"calibration_window_days", c.CalibrationWindowDays, 1, 3650},
	} {
		if r.value < r.lo || r.value > r.hi {
			return fmt.Errorf("tuning.%s must be %d-%d, got %d", r.key, r.lo, r.hi, r.value)
		}
	}
	return nil
}
