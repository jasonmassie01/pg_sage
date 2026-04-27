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
	MinPGVersion    int    // 0 = all versions
	Description     string // human-readable explanation
}

// SQLParser abstracts DDL classification so a future pg_query_go
// implementation can replace the regex-based classifier.
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
}
