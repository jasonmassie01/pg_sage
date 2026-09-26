package retention

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
)

const batchSize = 1000

// defaultRunInterval is used by RunEvery when no positive interval is given.
const defaultRunInterval = time.Hour

// Cleaner performs periodic data retention cleanup of one database's
// sage schema. It is safe to construct one per monitored database:
//
//	cleaner := retention.New(dbPool, cfg, logFn)
//	go cleaner.RunEvery(ctx, time.Hour)
//
// (fleet/meta-db wiring in cmd/pg_sage_sidecar is owned by the runtime
// area; see C08/G1-B04.)
type Cleaner struct {
	pool  *pgxpool.Pool
	cfg   *config.Config
	logFn func(string, string, ...any)
}

// New creates a new retention Cleaner.
func New(
	pool *pgxpool.Pool,
	cfg *config.Config,
	logFn func(string, string, ...any),
) *Cleaner {
	return &Cleaner{pool: pool, cfg: cfg, logFn: logFn}
}

// purgeRule deletes rows of sage.<table> whose timeCol is older than
// days. extra is a constant SQL predicate (never user input) that keeps
// rows which must survive, e.g. parents of NOT NULL references.
type purgeRule struct {
	table   string
	timeCol string
	days    int
	extra   string
}

// Keep predicates for parents referenced by NOT NULL foreign keys. The
// value ledger (incident_avoided) is evidence of prevented incidents and
// keeps its action/decision/verification; active leases keep decisions.
const (
	keepActionLog = `AND NOT EXISTS (SELECT 1 FROM sage.incident_avoided ia
	                   WHERE ia.action_log_id = action_log.id)`
	keepVerification = `AND verdict NOT IN ('pending', 'extended')
	    AND NOT EXISTS (SELECT 1 FROM sage.incident_avoided ia
	                   WHERE ia.verification_id = verification.id)`
	keepDecision = `AND (deadline_hard_at IS NULL OR deadline_hard_at < now()
	                   OR resolved_at IS NOT NULL)
	    AND NOT EXISTS (SELECT 1 FROM sage.verification v WHERE v.decision_id = decision.id)
	    AND NOT EXISTS (SELECT 1 FROM sage.change_lease cl WHERE cl.decision_id = decision.id)
	    AND NOT EXISTS (SELECT 1 FROM sage.incident_avoided ia
	                   WHERE ia.decision_id = decision.id)`
)

// purgeRules lists every sage time-series and its retention window
// (G7-B12, G4-B26). Order matters only for NOT NULL children, which are
// purged before their parents (verification and change_lease before
// decision). Nullable references are ON DELETE SET NULL (schema
// migration), so the other purges cannot block each other.
func purgeRules(cfg *config.Config) []purgeRule {
	r := cfg.Retention
	return []purgeRule{
		{"snapshots", "collected_at", r.SnapshotsDays, ""},
		{"query_store", "captured_at", r.SnapshotsDays, ""},
		{"health_history", "recorded_at", r.SnapshotsDays, ""},
		{"size_history", "collected_at", r.SnapshotsDays, ""},
		{"findings", "last_seen", r.FindingsDays, "AND status = 'resolved'"},
		{"incidents", "last_detected_at", r.FindingsDays, "AND resolved_at IS NOT NULL"},
		{"briefings", "generated_at", r.FindingsDays, ""},
		{"alert_log", "sent_at", r.ActionsDays, ""},
		{"notification_log", "sent_at", r.ActionsDays, ""},
		{"action_log", "executed_at", r.ActionsDays, keepActionLog},
		{"verification", "created_at", r.ActionsDays, keepVerification},
		{"change_lease", "acquired_at", r.ActionsDays, "AND state <> 'active'"},
		{"decision", "created_at", r.ActionsDays, keepDecision},
		{"retention_run", "created_at", r.ActionsDays, ""},
		{"explain_cache", "captured_at", r.ExplainsDays, ""},
		{"explain_results", "created_at", r.ExplainsDays, ""},
	}
}

// retentionExemptions documents sage tables with time columns that are
// intentionally NOT purged by age. A new time-series table must be added
// to purgeRules or here (enforced by a test).
var retentionExemptions = map[string]string{
	"action_queue":           "approval queue; lifecycle/expiry owned by the executor",
	"config":                 "current configuration, not a time-series",
	"config_audit":           "security audit trail of configuration changes",
	"crypto_meta":            "key metadata, not a time-series",
	"databases":              "fleet registry, not a time-series",
	"incident_avoided":       "value ledger; low volume, kept as evidence",
	"migration_run":          "low-volume migration evidence ledger",
	"notification_channels":  "configuration",
	"notification_rules":     "configuration",
	"policy":                 "standing policy, versioned configuration",
	"query_hints":            "active hints, current state",
	"rollout_run":            "low-volume rollout evidence ledger",
	"schema_baseline":        "current state, one row per object",
	"schema_findings":        "legacy table superseded by findings (v0.11); no writer",
	"sessions":               "expired sessions are deleted by auth's session cleaner",
	"slot_consumer_registry": "current state, one row per slot",
	"table_contract":         "declared contracts, current state",
	"toil_model":             "model configuration",
	"users":                  "accounts, not a time-series",
}

// Run performs batched deletes of expired data from all sage tables.
// A failing table is logged at ERROR and does not stop the others.
func (c *Cleaner) Run(ctx context.Context) {
	for _, rule := range purgeRules(c.cfg) {
		if ctx.Err() != nil {
			return
		}
		c.purgeTable(ctx, rule.table, rule.timeCol, rule.days, rule.extra)
	}
	c.cleanStaleFirstSeen(ctx)
}

// RunEvery runs the cleaner immediately and then every interval until
// ctx is cancelled. A non-positive interval uses one hour.
func (c *Cleaner) RunEvery(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultRunInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		c.Run(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// purgeTable deletes expired rows in batches of batchSize until none remain.
func (c *Cleaner) purgeTable(
	ctx context.Context,
	table string,
	timeCol string,
	retentionDays int,
	extraWhere string,
) {
	if retentionDays <= 0 {
		return
	}

	totalDeleted := int64(0)
	for {
		query := fmt.Sprintf(
			`DELETE FROM sage.%s
			 WHERE ctid IN (
				 SELECT ctid FROM sage.%s
				 WHERE %s < now() - make_interval(days => $1)
				 %s
				 LIMIT %d
			 )`,
			table, table, timeCol, extraWhere, batchSize,
		)

		tag, err := c.pool.Exec(ctx, query, retentionDays)
		if err != nil {
			c.logFn("ERROR", "retention: purging sage.%s failed after %d rows: %v",
				table, totalDeleted, err)
			return
		}

		deleted := tag.RowsAffected()
		totalDeleted += deleted

		if deleted < batchSize {
			break
		}
	}

	if totalDeleted > 0 {
		c.logFn("INFO", "retention: purged %d rows from sage.%s (retention: %d days)",
			totalDeleted, table, retentionDays)
	}
}

// cleanStaleFirstSeen removes first_seen:* entries from sage.config
// for indexes that no longer exist in the latest snapshot.
func (c *Cleaner) cleanStaleFirstSeen(ctx context.Context) {
	// Collect all first_seen keys from config.
	rows, err := c.pool.Query(ctx,
		`/* pg_sage */ SELECT key FROM sage.config WHERE key LIKE 'first_seen:%'`,
	)
	if err != nil {
		c.logFn("ERROR",
			"error reading first_seen keys: %v", err,
		)
		return
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			continue
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		c.logFn("ERROR",
			"error iterating first_seen keys: %v", err,
		)
		return
	}

	if len(keys) == 0 {
		return
	}

	// Get current index names from the latest snapshot.
	currentIndexes := make(map[string]bool)
	idxRows, err := c.pool.Query(ctx,
		`/* pg_sage */ SELECT DISTINCT data->>'indexrelname'
		 FROM sage.snapshots
		 WHERE category = 'indexes'
		   AND collected_at = (
			   SELECT max(collected_at) FROM sage.snapshots
			   WHERE category = 'indexes'
		   )`,
	)
	if err != nil {
		c.logFn("ERROR",
			"error reading latest index snapshot: %v", err,
		)
		return
	}
	defer idxRows.Close()

	for idxRows.Next() {
		var name string
		if err := idxRows.Scan(&name); err != nil {
			continue
		}
		currentIndexes["first_seen:"+name] = true
	}

	// Delete config entries for indexes no longer present. first_seen:*
	// keys are orphaned legacy data (nothing writes or reads them), so
	// removing them when the index snapshot is empty is harmless cleanup,
	// which is this function's purpose (the D1 audit "danger" was benign).
	deleted := 0
	for _, key := range keys {
		if currentIndexes[key] {
			continue
		}
		_, err := c.pool.Exec(ctx,
			"DELETE FROM sage.config WHERE key = $1", key,
		)
		if err != nil {
			c.logFn("ERROR",
				"error deleting stale config key %s: %v", key, err,
			)
			continue
		}
		deleted++
	}

	if deleted > 0 {
		c.logFn("INFO",
			"cleaned %d stale first_seen entries from sage.config",
			deleted,
		)
	}
}
