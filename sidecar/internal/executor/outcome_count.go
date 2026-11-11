package executor

import (
	"context"
	"strings"
	"sync"
	"time"
	"weak"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/policy"
)

// The outcome writers that only hold a pool (the rollback monitor, the
// verification settle path, agent guard contracts) count under the name
// of the database that pool connects to. An executor names its pool when
// it is given its database name, so the label is always the database
// whose sage.action_log holds the row; a pool no executor named (an
// embedder's, the control database's) counts nothing.
//
// A database can have more than one live pool: a reload prepares a
// replacement runtime beside the running one, and a replacement that is
// discarded (a failed health check or commit) must not stop the running
// runtime from counting, nor may the draining one go uncounted. So a
// binding never evicts another pool of the same name. Pools are held
// weakly: a pool no runtime holds any more drops out once it is
// collected, and PruneCounts drops every pool of a database that left.

var metricsPools = struct {
	mu     sync.RWMutex
	byPool map[weak.Pointer[pgxpool.Pool]]string
}{byPool: map[weak.Pointer[pgxpool.Pool]]string{}}

// bindMetricsPool names pool's database for /metrics.
func bindMetricsPool(pool *pgxpool.Pool, database string) {
	if pool == nil || database == "" {
		return
	}
	metricsPools.mu.Lock()
	defer metricsPools.mu.Unlock()
	for p := range metricsPools.byPool {
		if p.Value() == nil {
			delete(metricsPools.byPool, p)
		}
	}
	metricsPools.byPool[weak.Make(pool)] = database
}

// unbindMetricsDatabase forgets every pool of a database that left the
// process.
func unbindMetricsDatabase(database string) {
	metricsPools.mu.Lock()
	defer metricsPools.mu.Unlock()
	for p, name := range metricsPools.byPool {
		if name == database || p.Value() == nil {
			delete(metricsPools.byPool, p)
		}
	}
}

// metricsDatabaseFor is the database name pool was bound to ("" if none).
func metricsDatabaseFor(pool *pgxpool.Pool) string {
	if pool == nil {
		return ""
	}
	metricsPools.mu.RLock()
	defer metricsPools.mu.RUnlock()
	return metricsPools.byPool[weak.Make(pool)]
}

// RecordPoolOutcome counts an outcome written through pool, for writers
// that know the action type (agent guard contracts). Call it after the
// write committed.
func RecordPoolOutcome(pool *pgxpool.Pool, actionType, outcome string) {
	RecordOutcome(metricsDatabaseFor(pool), OutcomeClass(actionType, ""), outcome)
}

// classReadTimeout bounds the read of a counted action's type.
const classReadTimeout = 2 * time.Second

// countActionOutcome counts action actionID's committed outcome. It reads
// the action's type for the class only when the outcome is terminal and
// the pool's database is named; a failed read counts the class as other,
// so the outcome itself is never lost.
func countActionOutcome(ctx context.Context, pool *pgxpool.Pool, actionID int64,
	outcome string) {
	if _, terminal := outcomeLabel(outcome); !terminal {
		return
	}
	if database := metricsDatabaseFor(pool); database != "" {
		RecordOutcome(database, actionClass(ctx, pool, actionID), outcome)
	}
}

// countActionVerdict counts action actionID's finalized verification
// verdict.
func countActionVerdict(ctx context.Context, pool *pgxpool.Pool, actionID int64,
	verdict string) {
	if database := metricsDatabaseFor(pool); database != "" {
		RecordVerdict(database, actionClass(ctx, pool, actionID), verdict)
	}
}

// actionClass reads the action's class. It runs after the write, so it
// does not inherit a context the write's caller is about to cancel.
func actionClass(ctx context.Context, pool *pgxpool.Pool, actionID int64) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), classReadTimeout)
	defer cancel()
	var actionType, sql string
	err := pool.QueryRow(ctx, `/* pg_sage executor */ SELECT action_type, sql_executed
		FROM sage.action_log WHERE id = $1`, actionID).Scan(&actionType, &sql)
	if err != nil {
		return classOther
	}
	return OutcomeClass(actionType, sql)
}

// OutcomeClass is the policy change class of an action_log row, from its
// action type (the policy's or the one categorizeAction records) and, for
// the catch-all "alter", from its statement. Anything else is other.
func OutcomeClass(actionType, sql string) string {
	if class := changeClassForActionType(actionType); class != "" {
		return class
	}
	switch actionType {
	case "create_index", "drop_index", "reindex":
		return string(policy.ChangeIndex)
	case "create_statistics", "drop_statistics", "analyze":
		return string(policy.ChangeAnalyze)
	case "vacuum":
		return string(policy.ChangeVacuum)
	case "alter":
		return alterClass(sql)
	}
	return classOther
}

// alterClass classifies an ALTER statement: a setting (ALTER SYSTEM or
// ALTER DATABASE), a table's autovacuum options, or another table change.
func alterClass(sql string) string {
	s := strings.Join(strings.Fields(strings.ToUpper(stripLeadingComment(sql))), " ")
	switch {
	case strings.HasPrefix(s, "ALTER SYSTEM "), strings.HasPrefix(s, "ALTER DATABASE "):
		return string(policy.ChangeConfigGUC)
	case strings.HasPrefix(s, "ALTER TABLE ") && strings.Contains(s, "AUTOVACUUM_"):
		return string(policy.ChangeAutovacuumTuning)
	case strings.HasPrefix(s, "ALTER TABLE "):
		return string(policy.ChangeSchemaChange)
	}
	return classOther
}

// stripLeadingComment drops a leading /* ... */ tag.
func stripLeadingComment(sql string) string {
	s := strings.TrimSpace(sql)
	if strings.HasPrefix(s, "/*") {
		if end := strings.Index(s, "*/"); end >= 0 {
			return s[end+2:]
		}
	}
	return s
}
