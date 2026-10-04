package optimizer

// Recommendation is a validated index recommendation from the optimizer.
type Recommendation struct {
	Table      string  `json:"table"`
	DDL        string  `json:"ddl"`
	DropDDL    string  `json:"drop_ddl,omitempty"`
	Rationale  string  `json:"rationale"`
	Severity   string  `json:"severity"`
	Confidence float64 `json:"confidence"`
	IndexType  string  `json:"index_type"`
	Category   string  `json:"category"`
	// IndexCategory is the LLM label; Category is fixed (missing_index).
	IndexCategory           string   `json:"index_category,omitempty"`
	AffectedQueries         []string `json:"affected_queries,omitempty"`
	AffectedQueryIDs        []int64  `json:"affected_query_ids,omitempty"`
	EstimatedImprovementPct float64  `json:"estimated_improvement_pct"`
	Validated               bool     `json:"validated"`
	// WhatIf is the HypoPG verdict (WhatIfVerified / WhatIfUnverified);
	// WhatIfReason says why a recommendation is unverified.
	WhatIf       string `json:"what_if_verdict,omitempty"`
	WhatIfReason string `json:"what_if_reason,omitempty"`
	// PartitionedParent marks an index on a partitioned table: it is
	// advisory, and PartitionPlan holds the ON ONLY / per-partition /
	// ATTACH statements (nil for multi-level partitioning).
	PartitionedParent bool     `json:"partitioned_parent,omitempty"`
	PartitionPlan     []string `json:"partition_plan,omitempty"`
	// ActionLevel is the confidence tier: safe, moderate, high_risk.
	ActionLevel  string        `json:"action_level"`
	ActionRisk   string        `json:"action_risk,omitempty"` // safe, moderate, high_risk
	CostEstimate *CostEstimate `json:"cost_estimate,omitempty"`
}

// Result holds the output of one optimizer cycle.
type Result struct {
	TablesAnalyzed  int
	Recommendations []Recommendation
	Rejections      int
	TokensUsed      int
	PlanSource      string
	BudgetExhausted bool
	// MemorySkips counts LLM candidates whose what-if was skipped because
	// rejection memory already measured the same idea on this workload.
	MemorySkips int
}

// TableContext holds enriched per-table data for the LLM prompt.
type TableContext struct {
	Schema           string
	Table            string
	Columns          []ColumnInfo
	Indexes          []IndexInfo
	Queries          []QueryInfo
	Plans            []PlanSummary
	ColStats         []ColStat
	LiveTuples       int64
	DeadTuples       int64
	WriteRate        float64
	IndexCount       int
	TableBytes       int64
	IndexBytes       int64
	Workload         string // "oltp_write", "oltp_read", "olap", "htap"
	Collation        string
	Relpersistence   string // 'p' permanent, 'u' unlogged, 't' temp
	JoinPairs        []JoinPair
	IsPartitioned    bool // true if table is a partitioned parent (PG11+)
	IsPartitionChild bool // true if table is a child partition
	// PartitionChildren are a parent's direct partitions ("schema.table");
	// NestedPartitions is set when one of them is itself partitioned.
	PartitionChildren []string
	NestedPartitions  bool
	WriteRateKnown    bool // true when the table had recorded scan/write activity
	// MeasuredRejections are prompt lines for shapes HypoPG already
	// measured and rejected on this workload (rejection memory).
	MeasuredRejections []string
}

// ColumnInfo describes a table column.
type ColumnInfo struct {
	Name       string
	Type       string
	IsNullable bool
}

// IndexInfo describes an existing index on a table.
type IndexInfo struct {
	Name       string
	Definition string
	Scans      int64
	IsUnique   bool
	IsValid    bool
	SizeBytes  int64
}

// QueryInfo describes a query hitting a table.
type QueryInfo struct {
	QueryID     int64
	Text        string
	Calls       int64
	MeanTimeMs  float64
	TotalTimeMs float64
	Operators   []string // operators used in WHERE/JOIN (e.g., "@>", "&&", "@@")
}

// PlanSummary holds a condensed execution plan for one query.
type PlanSummary struct {
	QueryID          int64
	Summary          string
	ScanType         string
	HeapFetches      int64
	SortDisk         int64
	RowsRemoved      int64
	FilterExpression string // extracted filter expression from plan (e.g., "extract(year from col)")
}

// ColStat holds pg_stats data for a single column.
type ColStat struct {
	Column          string
	NDistinct       float64
	Correlation     float64
	MostCommonVals  []string
	MostCommonFreqs []float64
}

// ConfidenceInput and ComputeConfidence live in confidence.go.
