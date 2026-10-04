package optimizer

// Recommendation is a validated index recommendation from the optimizer.
type Recommendation struct {
	Table   string `json:"table"`
	DDL     string `json:"ddl"`
	DropDDL string `json:"drop_ddl,omitempty"`
	// Alongside are in-flight index DDLs on the same table (queued or
	// proposed, not built yet) the what-if creates as hypothetical indexes
	// before measuring this one.
	Alongside []string `json:"-"`
	Rationale string   `json:"rationale"`
	Severity  string   `json:"severity"`
	IndexType string   `json:"index_type"`
	Category  string   `json:"category"`
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
	PartitionedParent bool          `json:"partitioned_parent,omitempty"`
	PartitionPlan     []string      `json:"partition_plan,omitempty"`
	ActionRisk        string        `json:"action_risk,omitempty"` // safe, moderate, high_risk
	CostEstimate      *CostEstimate `json:"cost_estimate,omitempty"`
}

// TableContext holds a table's enriched data: what the tuning agent's tools
// show and what admission validates and measures a candidate against.
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
	// PlanSource is where Plans came from: auto_explain, generic_plan,
	// query_text_only (no plan captured) or none (not attempted).
	PlanSource string
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
