package tuner

import "time"

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
	// Advisory plan anti-patterns (market-10): no hint is prescribed.
	SymptomIndexFilterInefficient SymptomKind = "index_filter_inefficient"
	SymptomORJoinFilter           SymptomKind = "or_join_filter"
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
	// JoinAliases are the aliases of the relations a join node joins
	// (from its children; SubPlans/InitPlans excluded).
	JoinAliases []string
	Detail      map[string]any
}

// Prescription maps a symptom to a pg_hint_plan directive or
// (for SymptomStaleStats) an ANALYZE target identifier.
type Prescription struct {
	Symptom       SymptomKind
	HintDirective string
	Rationale     string
	// AnalyzeTarget is the canonical "schema.table" identifier
	// for SymptomStaleStats. Empty for all other symptoms.
	AnalyzeTarget string
}

// HintPlanAvailability describes pg_hint_plan detection results
// (tuning-04). A row in hint_plan.hints changes an application's plan only
// when ApplicationEffect holds: Preload, HintTableOn (for the same
// sessions), HintTableReady and Privilege. SessionLoad (LOAD into pg_sage's
// own backend) serves only pg_sage's own hinted EXPLAIN.
type HintPlanAvailability struct {
	// SharedPreload: shared_preload_libraries lists pg_hint_plan.
	SharedPreload bool `json:"shared_preload"`
	// Preload: shared or session_preload_libraries (server, database or
	// role) load pg_hint_plan into application sessions.
	Preload bool `json:"preload"`
	// HintTableOn: pg_hint_plan.enable_hint_table, and the module's master
	// switch pg_hint_plan.enable_hint, are on for application sessions
	// (server, ALTER DATABASE or ALTER ROLE).
	HintTableOn bool `json:"hint_table_on"`
	// HintTableReady: hint_plan.hints has the 1.7 layout with its unique
	// index, and pg_sage can read it.
	HintTableReady bool `json:"hint_table_ready"`
	// Privilege: pg_sage holds INSERT and DELETE on hint_plan.hints.
	Privilege bool `json:"privilege"`
	// ApplicationEffect: all four prerequisites hold for one kind of session.
	ApplicationEffect bool `json:"application_effect"`
	SessionLoad       bool `json:"session_load"`
	// Method: "shared_preload", "session_preload", "session_load", "unavailable".
	Method string `json:"method"`
	// Reasons name the missing prerequisites (see Readiness).
	Reasons []string `json:"reasons,omitempty"`
	// CheckedAt is when the facts were read; zero for facts a caller
	// supplied, which the tuner never re-reads.
	CheckedAt time.Time `json:"checked_at,omitzero"`
}

// TunerConfig holds per-query tuner settings.
type TunerConfig struct {
	Enabled                bool    `yaml:"enabled"`
	WorkMemMaxMB           int     `yaml:"work_mem_max_mb"`
	PlanTimeRatio          float64 `yaml:"plan_time_ratio"`
	NestedLoopRowThreshold int64   `yaml:"nested_loop_row_threshold"`
	ParallelMinTableRows   int64   `yaml:"parallel_min_table_rows"`
	MinQueryCalls          int     `yaml:"min_query_calls"`
	VerifyAfterApply       bool    `yaml:"verify_after_apply"`
	CascadeCooldownCycles  int     `yaml:"cascade_cooldown_cycles"`
	// PlanMaxAgeMinutes: a cached plan this old is stale (tuning-08);
	// 0 or less means 1440.
	PlanMaxAgeMinutes int `yaml:"plan_max_age_minutes"`
	// ParamShapeMinVariants: queryids of one collapsed IN-list shape before
	// it gets parameterization advice (tuning-11); 0 or less means 20.
	ParamShapeMinVariants int `yaml:"param_shape_min_variants"`
	// HintCostTolerancePct: the estimated cost rise a join or scan hint may
	// cause in the hinted EXPLAIN before it is refused (tuning-05). Taken
	// as given: 0 refuses any rise.
	HintCostTolerancePct float64 `yaml:"hint_cost_tolerance_pct"`

	// Feature 1 — Hint revalidation loop (v0.8.5).
	HintRetirementDays        int `yaml:"hint_retirement_days"`
	RevalidationIntervalHours int `yaml:"revalidation_interval_hours"`

	// Feature 2 — Stale-stats detection + ANALYZE (v0.8.5).
	StaleStatsEstimateSkew float64 `yaml:"stale_stats_estimate_skew"`
	StaleStatsModRatio     float64 `yaml:"stale_stats_mod_ratio"`
	StaleStatsAgeMinutes   int     `yaml:"stale_stats_age_minutes"`
	AnalyzeMaxTableMB      int64   `yaml:"analyze_max_table_mb"`
	AnalyzeCooldownMinutes int     `yaml:"analyze_cooldown_minutes"`
	AnalyzeTimeoutMs       int     `yaml:"analyze_timeout_ms"`
	MaxConcurrentAnalyze   int     `yaml:"max_concurrent_analyze"`
}
