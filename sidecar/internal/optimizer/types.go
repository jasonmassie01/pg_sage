package optimizer

import "github.com/pg-sage/sidecar/internal/verify"

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
	// WhatIfMeasured counts the statements the what-if planned with and
	// without the index; WhatIfUnplannedQueryIDs are those it could not.
	WhatIfMeasured          int     `json:"what_if_measured,omitempty"`
	WhatIfUnplannedQueryIDs []int64 `json:"what_if_unplanned_queryids,omitempty"`
	// PartitionedParent marks an index on a partitioned table: it is
	// advisory, and PartitionPlan holds the ON ONLY / per-partition /
	// ATTACH statements (nil for multi-level partitioning).
	PartitionedParent bool          `json:"partitioned_parent,omitempty"`
	PartitionPlan     []string      `json:"partition_plan,omitempty"`
	ActionRisk        string        `json:"action_risk,omitempty"` // safe, moderate, high_risk
	CostEstimate      *CostEstimate `json:"cost_estimate,omitempty"`
	// Replaces is the existing index this candidate replaces (nil for a
	// plain create): the per-table index cap does not count it.
	Replaces *IndexRef `json:"-"`
	// Ladder are verification-ladder records a producer attached (the
	// floor's clone rehearsal, lab-04a); the finding carries them.
	Ladder []verify.LadderRecord `json:"-"`
}

// IndexRef names an existing index of the table and its identity (OID).
type IndexRef struct {
	Name string
	OID  uint32
}

// TableContext holds a table's enriched data: what the tuning agent's tools
// show and what admission validates and measures a candidate against.
type TableContext struct {
	Schema     string
	Table      string
	Columns    []ColumnInfo
	Indexes    []IndexInfo
	Queries    []QueryInfo
	ColStats   []ColStat
	LiveTuples int64
	DeadTuples int64
	// WriteRate is the percentage of index-maintaining writes (inserts and
	// non-HOT updates) among those writes plus the tuples read.
	WriteRate float64
	// IndexCount counts the table's valid indexes.
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
	// KeyDefaults are each column's default collation and operator classes
	// (nil when the catalog was not read): the duplicate check drops an
	// explicit default before comparing with pg_get_indexdef.
	KeyDefaults map[string]KeyDefault
	// ColumnsKnown is true when Columns is the table's column list read
	// this cycle. When the read failed or returned nothing, a column the
	// candidate names is not evidence of a hallucination: the existence
	// check is skipped and HypoPG and PostgreSQL validate the columns.
	ColumnsKnown bool
	// PGMajor is the server's major version (18 for 18.1), read with the
	// collation; 0 when unknown, which no version-specific rule matches.
	PGMajor int
	// LeadingKeyStats are the pg_stats rows of the leading keys of the
	// table's multi-key btree indexes that ColStats lacks, read on PG 18+
	// only, for the skip-scan rule (market-15b).
	LeadingKeyStats []ColStat
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
	OID        uint32 // pg_class oid; zero when the snapshot lacks it
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

// CostEstimate is the measured cost of an index: the size HypoPG
// estimated for it.
type CostEstimate struct {
	EstimatedSizeBytes int64
}

// ColStat holds pg_stats data for a single column.
type ColStat struct {
	Column          string
	NDistinct       float64
	Correlation     float64
	MostCommonVals  []string
	MostCommonFreqs []float64
}
