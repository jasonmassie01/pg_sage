package tuner

// SymptomKind identifies a plan-level performance symptom.
type SymptomKind string

const (
	SymptomDiskSort         SymptomKind = "disk_sort"
	SymptomHashSpill        SymptomKind = "hash_spill"
	SymptomHighPlanTime     SymptomKind = "high_plan_time"
	SymptomBadNestedLoop    SymptomKind = "bad_nested_loop"
	SymptomSeqScanWithIndex SymptomKind = "seq_scan_with_index"
	SymptomParallelDisabled SymptomKind = "parallel_disabled"
	SymptomSortLimit        SymptomKind = "sort_limit"
	SymptomStatTempSpill    SymptomKind = "stat_temp_spill"
	SymptomStaleStats       SymptomKind = "stale_stats"
)

// PlanSymptom is a detected performance issue in an EXPLAIN plan.
type PlanSymptom struct {
	Kind         SymptomKind
	NodeType     string
	NodeDepth    int
	RelationName string
	Schema       string
	Alias        string
	IndexName    string
	Detail       map[string]any
}

// Prescription maps a symptom to a pg_hint_plan directive or
// (for SymptomStaleStats) an ANALYZE target identifier.
type Prescription struct {
	Symptom          SymptomKind
	HintDirective    string
	Rationale        string
	SuggestedRewrite string
	RewriteRationale string
	// AnalyzeTarget is the canonical "schema.table" identifier
	// for SymptomStaleStats. Empty for all other symptoms.
	AnalyzeTarget string
}

// HintPlanAvailability describes pg_hint_plan detection results.
type HintPlanAvailability struct {
	SharedPreload  bool
	SessionLoad    bool
	HintTableReady bool
	Available      bool
	Method         string // "shared_preload", "session_load", "unavailable"
}

// TunerConfig holds per-query tuner settings.
type TunerConfig struct {
	Enabled                bool    `yaml:"enabled"`
	LLMEnabled             bool    `yaml:"llm_enabled"`
	WorkMemMaxMB           int     `yaml:"work_mem_max_mb"`
	PlanTimeRatio          float64 `yaml:"plan_time_ratio"`
	NestedLoopRowThreshold int64   `yaml:"nested_loop_row_threshold"`
	ParallelMinTableRows   int64   `yaml:"parallel_min_table_rows"`
	MinQueryCalls          int     `yaml:"min_query_calls"`
	VerifyAfterApply       bool    `yaml:"verify_after_apply"`
	CascadeCooldownCycles  int     `yaml:"cascade_cooldown_cycles"`

	// Feature 1 — Hint revalidation loop (v0.8.5).
	HintRetirementDays           int     `yaml:"hint_retirement_days"`
	RevalidationIntervalHours    int     `yaml:"revalidation_interval_hours"`
	RevalidationKeepRatio        float64 `yaml:"revalidation_keep_ratio"`
	RevalidationRollbackRatio    float64 `yaml:"revalidation_rollback_ratio"`
	RevalidationExplainTimeoutMs int     `yaml:"revalidation_explain_timeout_ms"`

	// Feature 2 — Stale-stats detection + ANALYZE (v0.8.5).
	StaleStatsEstimateSkew float64 `yaml:"stale_stats_estimate_skew"`
	StaleStatsModRatio     float64 `yaml:"stale_stats_mod_ratio"`
	StaleStatsAgeMinutes   int     `yaml:"stale_stats_age_minutes"`
	AnalyzeMaxTableMB      int64   `yaml:"analyze_max_table_mb"`
	AnalyzeCooldownMinutes int     `yaml:"analyze_cooldown_minutes"`
	AnalyzeTimeoutMs       int     `yaml:"analyze_timeout_ms"`
	MaxConcurrentAnalyze   int     `yaml:"max_concurrent_analyze"`
}
