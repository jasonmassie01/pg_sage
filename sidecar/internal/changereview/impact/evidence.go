package impact

import (
	"strconv"
	"time"
)

// Evidence data. Every finding cites its statement ("stmt:<n>") and the
// facts below; ids are "<kind>:<ref>" with the kinds named here. All of
// it is stored canonical and digested (changereview.NewEvidence).

// stmtData is "stmt:<n>": one statement of the input.
type stmtData struct {
	Index              int      `json:"index"`
	File               string   `json:"file,omitempty"`
	Line               int      `json:"line,omitempty"`
	Kind               string   `json:"kind"`
	SQL                string   `json:"sql"`
	Relations          []string `json:"relations,omitempty"`
	LockMode           string   `json:"lock_mode,omitempty"`
	InTransactionBlock bool     `json:"in_transaction_block,omitempty"`
	LockTimeoutMs      *int64   `json:"lock_timeout_ms,omitempty"`
	ParseMode          string   `json:"parse_mode"`
}

// tableData is "table:<schema.name>": one touched relation's live context.
type tableData struct {
	Relation     string   `json:"relation"`
	Kind         string   `json:"kind"`
	RelTuples    float64  `json:"reltuples"`
	TotalBytes   int64    `json:"total_bytes"`
	HeapBytes    int64    `json:"heap_bytes"`
	Partitions   int      `json:"partitions,omitempty"`
	Children     int      `json:"inheritance_children,omitempty"`
	Publications []string `json:"publications,omitempty"`
	RateKnown    bool     `json:"write_rate_known"`
	InsPerS      float64  `json:"inserts_per_s"`
	UpdPerS      float64  `json:"updates_per_s"`
	DelPerS      float64  `json:"deletes_per_s"`
	RateSince    string   `json:"write_rate_since,omitempty"`
}

// queryData is "queryid:<id>": one workload statement touching a relation.
type queryData struct {
	QueryID   int64             `json:"queryid"`
	Query     string            `json:"query"`
	Calls     int64             `json:"calls"`
	Calls7d   int64             `json:"calls_7d"`
	PeakPerS  float64           `json:"peak_calls_per_s"`
	RateKnown bool              `json:"rate_known"`
	LockMode  string            `json:"lock_mode"`
	Kind      string            `json:"kind"`
	Columns   []string          `json:"columns,omitempty"`
	Star      bool              `json:"select_star,omitempty"`
	Tags      map[string]string `json:"tags,omitempty"`
}

// riskData is "risk:<n>.<part>": CR-01, the classifier and RiskAssessor.
type riskData struct {
	RuleID          string  `json:"rule_id"`
	LockLevel       string  `json:"lock_level"`
	RequiresRewrite bool    `json:"requires_rewrite"`
	BinaryCoercible bool    `json:"binary_coercible,omitempty"`
	RiskScore       float64 `json:"risk_score"`
	EstimatedLockMs int64   `json:"estimated_lock_ms"`
	TableBytes      int64   `json:"table_size_bytes"`
	EstimatedRows   int64   `json:"estimated_rows"`
	SafeAlternative string  `json:"safe_alternative,omitempty"`
	Description     string  `json:"description,omitempty"`
}

// Sources of a CR-02 lock hold.
const (
	holdRiskAssessor = "risk_assessor"
	holdIndexBuild   = "index_build"
)

// lockQueueData is "lockq:<n>.<part>": CR-02's queue arithmetic.
type lockQueueData struct {
	Relation            string   `json:"relation"`
	LockMode            string   `json:"lock_mode"`
	ConflictingQueryIDs []int64  `json:"conflicting_queryids"`
	RateUnknownIDs      []int64  `json:"rate_unknown_queryids,omitempty"`
	PeakConflictingPerS float64  `json:"peak_conflicting_calls_per_s"`
	HoldMs              int64    `json:"hold_ms"`
	HoldSource          string   `json:"hold_source"`
	Queued              float64  `json:"queued_estimate"`
	LockTimeoutMs       *int64   `json:"lock_timeout_ms"`
	WaitingExposure     *float64 `json:"waiting_exposure"`
	NoLockTimeout       bool     `json:"no_lock_timeout"`
	Method              string   `json:"method"`
	Formula             string   `json:"formula"`
}

// viewData is "view:<schema.name>": a view depending on a relation.
type viewData struct {
	View     string   `json:"view"`
	Relation string   `json:"relation"`
	Columns  []string `json:"columns,omitempty"`
}

// functionData is "function:<schema.name>": a body naming a changed name.
type functionData struct {
	Function string   `json:"function"`
	Language string   `json:"language"`
	Names    []string `json:"names"`
}

// indexData is "index:<schema.name>": one existing index.
type indexData struct {
	Index            string     `json:"index"`
	Relation         string     `json:"relation"`
	Definition       string     `json:"definition"`
	Unique           bool       `json:"unique,omitempty"`
	Primary          bool       `json:"primary,omitempty"`
	Valid            bool       `json:"valid"`
	Scans            int64      `json:"idx_scan"`
	LastScan         *time.Time `json:"last_idx_scan,omitempty"`
	ConstraintBacked []string   `json:"constraint_backed,omitempty"`
	PlanUsers        []int64    `json:"plan_users,omitempty"`
	SupportsFKs      []string   `json:"supports_fks,omitempty"`
}

// nullsData is "nulls:<schema.table.column>": pg_stats for SET NOT NULL.
type nullsData struct {
	Relation          string  `json:"relation"`
	Column            string  `json:"column"`
	NullFrac          float64 `json:"null_frac"`
	RelTuples         float64 `json:"reltuples"`
	EstimatedNullRows float64 `json:"estimated_null_rows"`
}

// seqScan is one sequential scan node of a plan.
type seqScan struct {
	Relation string  `json:"relation"`
	Rows     float64 `json:"rows"`
}

// planData is "plan:<n>": a plan-only EXPLAIN of a statement or its read
// shape.
type planData struct {
	Statement int       `json:"statement"`
	Shape     string    `json:"shape"`
	Summary   string    `json:"summary"`
	Rows      float64   `json:"rows"`
	TotalCost float64   `json:"total_cost"`
	SeqScans  []seqScan `json:"seq_scans,omitempty"`
}

// whatIfData is "whatif:<n>.<part>": CR-09's HypoPG evaluation.
type whatIfData struct {
	DDL            string   `json:"ddl"`
	Method         string   `json:"method"`
	Available      bool     `json:"hypopg_available"`
	ImprovementPct *float64 `json:"improvement_pct,omitempty"`
	Measured       int      `json:"measured"`
	Failed         int      `json:"failed"`
	SizeBytes      int64    `json:"size_bytes,omitempty"`
	Verdict        string   `json:"verdict,omitempty"`
	Reason         string   `json:"reason,omitempty"`
	QueryIDs       []int64  `json:"queryids,omitempty"`
	WritesPerS     float64  `json:"table_writes_per_s"`
	Error          string   `json:"error,omitempty"`
}

// fkData is "fk:<n>.<part>": a new foreign key.
type fkData struct {
	Relation      string   `json:"relation"`
	Columns       []string `json:"columns"`
	Types         []string `json:"types"`
	RefRelation   string   `json:"ref_relation"`
	RefColumns    []string `json:"ref_columns"`
	RefTypes      []string `json:"ref_types"`
	CoveringIndex string   `json:"covering_index,omitempty"`
	RateKnown     bool     `json:"ref_write_rate_known"`
	RefUpdPerS    float64  `json:"ref_updates_per_s"`
	RefDelPerS    float64  `json:"ref_deletes_per_s"`
}

// publicationData is "publication:<name>".
type publicationData struct {
	Publication string `json:"publication"`
	Relation    string `json:"relation"`
}

// unknownData is "unknown:<slug>": what could not be read and why.
type unknownData struct {
	Subject string `json:"subject"`
	Reason  string `json:"reason"`
	Detail  string `json:"detail,omitempty"`
	Fix     string `json:"fix,omitempty"`
}

// expandData is "expand:<n>.<part>": migration/plan's expand/contract steps.
type expandData struct {
	Classification string     `json:"classification"`
	Steps          []stepData `json:"steps"`
}

type stepData struct {
	Phase string `json:"phase"`
	SQL   string `json:"sql"`
}

// txnData is "txn:<n>": the transaction a statement runs in.
type txnData struct {
	InTransactionBlock bool   `json:"in_transaction_block"`
	TransactionalFile  bool   `json:"transactional_file"`
	TransactionalFrom  string `json:"transactional_from,omitempty"`
	MigrationTool      string `json:"migration_tool,omitempty"`
}

func stmtEvidenceID(n int) string { return "stmt:" + strconv.Itoa(n) }

func partRef(n, part int) string { return strconv.Itoa(n) + "." + strconv.Itoa(part) }

func queryEvidenceID(id int64) string { return "queryid:" + strconv.FormatInt(id, 10) }

func tableEvidenceID(rel string) string { return "table:" + evidenceRef(rel) }
