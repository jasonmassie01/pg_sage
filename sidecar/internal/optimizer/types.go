package optimizer

// Recommendation is a validated index recommendation from the optimizer.
type Recommendation struct {
	Table                   string        `json:"table"`
	DDL                     string        `json:"ddl"`
	DropDDL                 string        `json:"drop_ddl,omitempty"`
	Rationale               string        `json:"rationale"`
	Severity                string        `json:"severity"`
	Confidence              float64       `json:"confidence"`
	IndexType               string        `json:"index_type"`
	Category                string        `json:"category"`
	AffectedQueries         []string      `json:"affected_queries,omitempty"`
	EstimatedImprovementPct float64       `json:"estimated_improvement_pct"`
	Validated               bool          `json:"validated"`
	ActionLevel             string        `json:"action_level"`            // autonomous, advisory, informational
	CostEstimate            *CostEstimate `json:"cost_estimate,omitempty"`
}

// Result holds the output of one optimizer cycle.
type Result struct {
	TablesAnalyzed  int
	Recommendations []Recommendation
	Rejections      int
	TokensUsed      int
	PlanSource      string
}

// TableContext holds enriched per-table data for the LLM prompt.
type TableContext struct {
	Schema     string
	Table      string
	Columns    []ColumnInfo
	Indexes    []IndexInfo
	Queries    []QueryInfo
	Plans      []PlanSummary
	ColStats   []ColStat
	LiveTuples int64
	DeadTuples int64
	WriteRate  float64
	IndexCount int
	TableBytes int64
	IndexBytes int64
	Workload   string // "oltp_write", "oltp_read", "olap", "htap"
	Collation      string
	Relpersistence string // 'p' permanent, 'u' unlogged, 't' temp
	JoinPairs      []JoinPair
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
