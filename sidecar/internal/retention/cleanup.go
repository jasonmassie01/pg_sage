package retention

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/rca"
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
	// control holds the sre_* coordination and ledger tables when they
	// live in a meta database; nil means the monitored database.
	control *pgxpool.Pool
}

// New creates a new retention Cleaner.
func New(
	pool *pgxpool.Pool,
	cfg *config.Config,
	logFn func(string, string, ...any),
) *Cleaner {
	return &Cleaner{pool: pool, cfg: cfg, logFn: logFn}
}

// WithControlPool prunes the control-database tables (controlTables) in
// pool instead of the monitored database: the meta database in meta-db
// mode. Nil keeps the monitored database.
func (c *Cleaner) WithControlPool(pool *pgxpool.Pool) *Cleaner {
	c.control = pool
	return c
}

// controlTables live beside the sre_* coordination tables (the control
// database), not necessarily in the monitored database.
var controlTables = map[string]bool{"sre_eval_runs": true}

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
// Credited actions (verified success with toil credit) are pg_sage's
// evidence of worth, so they and the verification that earned the credit
// are never purged (D3); uncredited and rolled-back rows still age out.
const (
	keepActionLog = `AND NOT (action_log.outcome = 'success'
	                   AND action_log.toil_minutes_saved IS NOT NULL)
	    AND NOT EXISTS (SELECT 1 FROM sage.incident_avoided ia
	                   WHERE ia.action_log_id = action_log.id)
	    AND NOT EXISTS (SELECT 1 FROM sage.recommendation r
	                   WHERE r.action_log_id = action_log.id
	                   AND r.state IN ('applied', 'verifying', 'failed'))`
	// Only terminal recommendations age out; their revisions and history
	// go with them (ON DELETE CASCADE). Live ones are kept however old.
	keepRecommendation = `AND state IN ('verified', 'reverted', 'inconclusive',
	                   'superseded', 'abandoned')`
	keepVerification = `AND verdict NOT IN ('pending', 'extended')
	    AND NOT EXISTS (SELECT 1 FROM sage.incident_avoided ia
	                   WHERE ia.verification_id = verification.id)
	    AND NOT EXISTS (SELECT 1 FROM sage.action_log al
	                   WHERE al.id = verification.action_log_id
	                   AND al.outcome = 'success'
	                   AND al.toil_minutes_saved IS NOT NULL)`
	keepDecision = `AND (deadline_hard_at IS NULL OR deadline_hard_at < now()
	                   OR resolved_at IS NOT NULL)
	    AND NOT EXISTS (SELECT 1 FROM sage.verification v WHERE v.decision_id = decision.id)
	    AND NOT EXISTS (SELECT 1 FROM sage.change_lease cl WHERE cl.decision_id = decision.id)
	    AND NOT EXISTS (SELECT 1 FROM sage.incident_avoided ia
	                   WHERE ia.decision_id = decision.id)`
	// Stored bench and game-day reports age out unless one is the evidence
	// of a current ledger level or a pending promotion (any "id" in that
	// evidence), or the newest report of a family for its source and
	// database: what LatestBench and the game-day evidence read (M7).
	keepEvalRun = `AND NOT EXISTS (SELECT 1 FROM sage.sre_family_autonomy a
	                   WHERE a.deployment_id = sre_eval_runs.deployment_id
	                   AND jsonb_path_exists(a.evidence, 'lax $.**.id ? (@ == $rid)',
	                       jsonb_build_object('rid', sre_eval_runs.id::text)))
	    AND NOT EXISTS (SELECT 1 FROM sage.sre_autonomy_proposals p
	                   WHERE p.deployment_id = sre_eval_runs.deployment_id
	                   AND p.status = 'pending'
	                   AND jsonb_path_exists(p.evidence, 'lax $.**.id ? (@ == $rid)',
	                       jsonb_build_object('rid', sre_eval_runs.id::text)))
	    AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(sre_eval_runs.cells) c
	                   WHERE NOT EXISTS (SELECT 1 FROM sage.sre_eval_runs n
	                       WHERE n.deployment_id = sre_eval_runs.deployment_id
	                       AND n.source = sre_eval_runs.source
	                       AND n.database_name IS NOT DISTINCT FROM
	                           sre_eval_runs.database_name
	                       AND (n.generated_at, n.ingested_at, n.id) >
	                           (sre_eval_runs.generated_at, sre_eval_runs.ingested_at,
	                            sre_eval_runs.id)
	                       AND n.cells @> jsonb_build_array(
	                           jsonb_build_object('family', c->'family'))))`
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
		{"briefings", "generated_at", r.FindingsDays, ""},
		{"alert_log", "sent_at", r.ActionsDays, ""},
		{"notification_log", "sent_at", r.ActionsDays, ""},
		{"action_log", "executed_at", r.ActionsDays, keepActionLog},
		{"verification", "created_at", r.ActionsDays, keepVerification},
		{"change_lease", "acquired_at", r.ActionsDays, "AND state <> 'active'"},
		{"decision", "created_at", r.ActionsDays, keepDecision},
		{"recommendation", "updated_at", r.ActionsDays, keepRecommendation},
		{"retention_run", "created_at", r.ActionsDays, ""},
		{"admission_withheld", "last_seen_at", r.ActionsDays, ""},
		{"explain_cache", "captured_at", r.ExplainsDays, ""},
		{"explain_results", "created_at", r.ExplainsDays, ""},
		// Used or expired SSO link grants are dead weight once old (D7).
		{"user_oidc_link_grants", "expires_at", r.ActionsDays, ""},
		{"sre_eval_runs", "ingested_at", cfg.SRE.Autonomy.ReportRetentionDays, keepEvalRun},
	}
}

// retentionExemptions documents sage tables with time columns that are
// intentionally NOT purged by age. A new time-series table must be added
// to purgeRules or here (enforced by a test).
var retentionExemptions = map[string]string{
	"action_queue":          "approval queue; lifecycle/expiry owned by the executor",
	"auth_audit":            "security audit trail of SSO link, unlink and grant use",
	"chatops_identities":    "admin-managed mapping of chat users to accounts, current state",
	"chatops_replay":        "pruned by chatops on every callback (24 h replay window)",
	"config":                "current configuration, not a time-series",
	"config_audit":          "security audit trail of configuration changes",
	"crypto_meta":           "key metadata, not a time-series",
	"databases":             "fleet registry, not a time-series",
	"incident_avoided":      "value ledger; low volume, kept as evidence",
	"io_rate_sample":        "pruned by the IO sampler (verify.io_sample_retention_days)",
	"incidents":             "pruned by rca.PruneResolvedIncidents (resolved_at, findings_days)",
	"migration_run":         "low-volume migration evidence ledger",
	"notification_channels": "configuration",
	"notification_rules":    "configuration",
	"policy":                "standing policy, versioned configuration",
	"query_hints":           "active hints, current state",
	"recommendation_revision": "immutable revisions; deleted with their terminal " +
		"recommendation (ON DELETE CASCADE, actions_days)",
	"recommendation_transition": "immutable history; deleted with its terminal " +
		"recommendation (ON DELETE CASCADE, actions_days)",
	"rollout_run": "low-volume rollout evidence ledger",
	"runway_samples": "runway series; the runway monitor prunes them on every pass " +
		"(sre.runways.sample_retention_hours)",
	"rollout_instance": "per-database steps of a rollout run; low volume",
	// Sage SRE M7: earned-autonomy evidence. Purging by age would quietly
	// lower or erase the evidence promotions rest on ("no harmful outcome
	// ever"), so these are kept like the other evidence ledgers.
	"sre_family_autonomy":    "current autonomy level, one row per family x class",
	"sre_autonomy_proposals": "promotion proposals and their human decisions (audit)",
	"sre_autonomy_events":    "append-only autonomy history (audit); updates are refused",
	"sre_autonomy_outcomes":  "append-only live outcomes; promotion evidence",
	"sre_packet_reviews":     "operator shadow reviews; promotion evidence, one per review",
	"sre_game_days":          "game-day runs; low volume (at most one per interval_hours)",
	"schema_baseline":        "current state, one row per object",
	"schema_findings":        "legacy table superseded by findings (v0.11); no writer",
	"sessions":               "expired sessions are deleted by auth's session cleaner",
	"slot_consumer_registry": "current state, one row per slot",
	"sre_change_events": "SRE change feed; aged out by its poller " +
		"(sre.change_events.retention_days)",
	"sre_change_feed_state": "SRE change feed cursors and snapshots, one row per source",
	"sre_service_slos":      "SLO definitions and current error-budget state, one row per SLO",
	"sre_sli_samples": "SLI counter samples; aged out by the SLO engine (the longest " +
		"SLO or burn window plus a day)",
	"sre_slo_transitions": "SLO state history; aged out by the SLO engine " +
		"(sre.timeline_retention_days)",
	"sre_action_proposals": "SRE action proposals; deleted with their investigation " +
		"(ON DELETE CASCADE) by sre retention",
	"sre_budget_reservations": "SRE model budget ledger; deleted with its investigation " +
		"by sre retention (sre.timeline_retention_days) unless the hold is unsettled",
	"sre_database_bindings": "stable SRE database identity, one row per database",
	"sre_deployments":       "the deployment identity, one row",
	"sre_events": "SRE event hash chain; deleted with its investigation by sre " +
		"retention (sre.timeline_retention_days)",
	"sre_evidence": "SRE evidence; aged out by sre retention (sre.evidence_retention_days, " +
		"pinned and live investigations kept, tombstoned)",
	"sre_hypotheses": "SRE hypotheses; deleted with their investigation by sre retention",
	"sre_investigations": "SRE investigations; aged out by sre retention " +
		"(sre.timeline_retention_days, pinned and live kept, tombstoned)",
	"sre_steps": "SRE investigation steps; deleted with their investigation by sre " +
		"retention",
	"sre_tombstones": "what sre retention deleted; one row per investigation and kind",
	"sre_runbooks":   "SRE runbooks, versioned configuration (retired, never deleted)",
	"sre_runbook_versions": "immutable SRE runbook versions and their signatures; the " +
		"audit trail of what was allowed to run",
	"sre_runbook_runs": "SRE runbook run history; deleted with its investigation " +
		"(ON DELETE CASCADE, sre.timeline_retention_days)",
	"sre_investigation_outcomes": "operator verdicts on SRE investigations; deleted with " +
		"their investigation (ON DELETE CASCADE, sre.timeline_retention_days)",
	"table_contract": "declared contracts, current state",
	"toil_model":     "model configuration",
	"users":          "accounts, not a time-series",
}

// Run performs batched deletes of expired data from all sage tables.
// A failing table is logged at ERROR and does not stop the others.
func (c *Cleaner) Run(ctx context.Context) {
	for _, rule := range purgeRules(c.cfg) {
		if ctx.Err() != nil {
			return
		}
		target := c
		if controlTables[rule.table] && c.control != nil {
			target = &Cleaner{pool: c.control, cfg: c.cfg, logFn: c.logFn}
		}
		target.purgeTable(ctx, rule.table, rule.timeCol, rule.days, rule.extra)
	}
	if ctx.Err() == nil {
		c.pruneIncidents(ctx)
	}
}

// pruneIncidents deletes incidents resolved more than findings_days ago
// through the RCA package's own hook, so the resolution-age rule lives in
// one place (substrate-B7). Open incidents are never deleted.
func (c *Cleaner) pruneIncidents(ctx context.Context) {
	days := c.cfg.Retention.FindingsDays
	if days <= 0 {
		return
	}
	window := time.Duration(days) * 24 * time.Hour
	deleted, err := rca.PruneResolvedIncidents(ctx, c.pool, window)
	if err != nil {
		c.logFn("ERROR", "retention: pruning resolved sage.incidents failed "+
			"after %d rows (retention: %d days): %v", deleted, days, err)
		return
	}
	if deleted > 0 {
		c.logFn("INFO", "retention: pruned %d resolved incidents "+
			"(retention: %d days)", deleted, days)
	}
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
