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

// Prescription maps a symptom to a pg_hint_plan directive.
type Prescription struct {
	Symptom          SymptomKind
	HintDirective    string
	Rationale        string
	SuggestedRewrite string
	RewriteRationale string
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
}
