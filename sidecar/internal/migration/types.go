package migration

// DDLClassification is the result of classifying a DDL statement against
// the rule catalog. Each matched rule produces one classification.
type DDLClassification struct {
	RuleID          string
	Statement       string
	LockLevel       string // ACCESS EXCLUSIVE, SHARE ROW EXCLUSIVE, SHARE, SHARE UPDATE EXCLUSIVE
	RequiresRewrite bool
	SafeAlternative string // suggested safe DDL
	TableName       string // extracted from statement
	SchemaName      string // extracted from statement
	Description     string // human-readable explanation
	ColumnName      string // ALTER COLUMN target, when extracted
	TargetType      string // ALTER COLUMN ... TYPE target, when extracted
}

// SQLParser abstracts DDL classification. RegexClassifier is the only
// implementation: it matches the rule catalog's regular expressions
// against sanitized, statement-split SQL. pgVersion is the server's
// server_version_num (0 when unknown).
type SQLParser interface {
	Classify(sql string, pgVersion int) []DDLClassification
}

// DDLRisk is the risk assessment for a single classified DDL statement,
// enriched with live database metrics.
type DDLRisk struct {
	Statement       string
	RuleID          string
	LockLevel       string
	RequiresRewrite bool
	TableName       string
	SchemaName      string
	TableSizeBytes  int64
	EstimatedRows   int64
	ActiveQueries   int
	LongestQuerySec float64
	PendingLocks    int
	ReplicationLag  float64
	RiskScore       float64 // 0.0-1.0
	SafeAlternative string
	EstimatedLockMs int64
	Description     string
	// StatsKnown is true when pg_class returned analyzed statistics
	// (reltuples >= 0) for the target table.
	StatsKnown bool
}
