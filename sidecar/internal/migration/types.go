package migration

import (
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

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
	// SessionRole is the role running the DDL, whose search_path resolves
	// an unqualified table ("" when unknown).
	SessionRole string
	// SessionPID is the backend running the DDL (0 when unknown), which
	// the activity read leaves out: the DDL is not load it competes with.
	SessionPID int
}

// SQLParser abstracts DDL classification. NewClassifier is the
// production implementation (the parse-tree classifier, or the regex rules
// on a build without cgo); RegexClassifier matches the rule catalog's
// regular expressions alone. Both work on sanitized, statement-split SQL.
// pgVersion is the server's server_version_num or major (0 when unknown).
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
	SessionPID      int // the DDL's own backend, 0 when unknown
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
	// NameAmbiguous is true when an unqualified table name matched several
	// schemas: stats are unknown rather than read from the wrong table.
	NameAmbiguous bool
	// Measured records which live reads succeeded. A failed read leaves
	// its value zero and its flag false, so the finding reports it as not
	// measured rather than as a passing zero (migration-01).
	Measured DDLMeasured
	// MeasuredAt is when the live reads ran; zero without a pool or when
	// every read failed.
	MeasuredAt time.Time
	// readErr joins the live reads that failed (a missing row is not a
	// failure: the table does not exist yet).
	readErr error
}

// DDLMeasured flags the live reads of one assessment that succeeded.
type DDLMeasured struct {
	TableStats     bool // reltuples and size from pg_class
	Activity       bool // active queries on the table
	PendingLocks   bool // ungranted locks on the table
	ReplicationLag bool // replay lag from pg_stat_replication
}

// any reports whether at least one live read succeeded.
func (m DDLMeasured) any() bool {
	return m.TableStats || m.Activity || m.PendingLocks || m.ReplicationLag
}

// noteReadErr keeps a failed live read; pgx.ErrNoRows (no such table) is
// an answer, not a failure.
func (r *DDLRisk) noteReadErr(err error) {
	if err == nil || errors.Is(err, pgx.ErrNoRows) {
		return
	}
	r.readErr = errors.Join(r.readErr, err)
}
