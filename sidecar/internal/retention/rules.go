package retention

import (
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/partition"
)

// purgeRule deletes rows of sage.<table> whose timeCol is older than
// days. extra is a constant SQL predicate (never user input) that keeps
// rows which must survive, e.g. parents of NOT NULL references; it names
// the row being purged by the table's name. batch bounds one statement
// (default batchSize). partitioned names a table partitioned by day: expired days are dropped, rows are deleted only from
// its history and default partitions.
type purgeRule struct {
	table       string
	timeCol     string
	days        int
	extra       string
	batch       int
	partitioned *partition.Table
	// sweepCol, when set, makes the rule swept (sweep.go): an incremental
	// pass reads only rows with sweepCol at or after its floor.
	sweepCol string
}

// Keep predicates for parents referenced by NOT NULL foreign keys. The
// value ledger (incident_avoided) is evidence of prevented incidents and
// keeps its action/decision/verification; active leases keep decisions.
// Credited actions (verified success with toil credit) are pg_sage's
// evidence of worth, so they and the verification that earned the credit
// are never purged (D3); uncredited and rolled-back rows still age out.
// keepSnapshotBases keeps a snapshot row while a row collected at or after
// from (an SQL expression) is built on it: a delta is only readable with
// its base, and a base may itself be a checkpoint on a keyframe (the
// writer's chains are at most two deep). A base used only by rows before
// from goes with them.
func keepSnapshotBases(from string) string {
	return `AND NOT EXISTS (SELECT 1 FROM sage.snapshots d
	                   WHERE d.base_id = snapshots.id
	                   AND (d.collected_at >= ` + from + `
	                        OR EXISTS (SELECT 1 FROM sage.snapshots d2
	                                   WHERE d2.base_id = d.id
	                                   AND d2.collected_at >= ` + from + `)))`
}

// keepSnapshotKeyframe keeps the bases of retained snapshots; $1 is the
// retention in days.
var keepSnapshotKeyframe = keepSnapshotBases("now() - make_interval(days => $1)")

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
	// OFFSET 0 keeps a keep check on a large table a correlated index probe
	// per candidate (a subplan): as an anti join, the generic plan of a
	// purge with many candidates hashed all of sage.action_log or
	// sage.verification on every run (perf gate, perf-selfexcl).
	keepVerification = `AND verdict NOT IN ('pending', 'extended')
	    AND NOT EXISTS (SELECT 1 FROM sage.incident_avoided ia
	                   WHERE ia.verification_id = verification.id)
	    AND NOT EXISTS (SELECT 1 FROM sage.action_log al
	                   WHERE al.id = verification.action_log_id
	                   AND al.outcome = 'success'
	                   AND al.toil_minutes_saved IS NOT NULL OFFSET 0)`
	keepDecision = `AND (deadline_hard_at IS NULL OR deadline_hard_at < now()
	                   OR resolved_at IS NOT NULL)
	    AND NOT EXISTS (SELECT 1 FROM sage.verification v WHERE v.decision_id = decision.id
	                   OFFSET 0)
	    AND NOT EXISTS (SELECT 1 FROM sage.change_lease cl WHERE cl.decision_id = decision.id)
	    AND NOT EXISTS (SELECT 1 FROM sage.incident_avoided ia
	                   WHERE ia.decision_id = decision.id)`
	// Non-execute decisions (withheld gate verdicts, schema guard
	// observations) are facts about candidates, not the record of a change:
	// they age out on decisions_days from when they were last seen. A row an
	// action, a verification, a lease or a value record points at is kept,
	// and so is a schema guard retention dry run (the evidence that
	// authorizes its later deletes); those age with the actions. The
	// created_at bound (implied by the window) lets the purge use
	// idx_decision_created.
	keepWithheldDecision = `AND verdict <> 'execute'
	    AND created_at < now() - make_interval(days => $1)
	    AND NOT (feature = 'schema_guard'
	             AND evidence->>'disposition' IS NOT DISTINCT FROM 'dry_run')
	    AND NOT EXISTS (SELECT 1 FROM sage.action_log al
	                   WHERE al.decision_id = decision.id) ` + keepDecision
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
	// The approval queue: pending, approved and retrying (failed, not yet
	// expired) proposals stay however old; decided ones age out unless an
	// action log, a decision or an SRE proposal still points at them.
	keepActionQueue = `AND (status IN ('rejected', 'expired', 'executed')
	         OR (status = 'failed' AND (expires_at IS NULL OR expires_at < now())))
	    AND NOT EXISTS (SELECT 1 FROM sage.action_log al
	                   WHERE al.id = action_queue.action_log_id)
	    AND NOT EXISTS (SELECT 1 FROM sage.decision d WHERE d.queue_id = action_queue.id)
	    AND NOT EXISTS (SELECT 1 FROM sage.sre_action_proposals p
	                   WHERE p.queue_id = action_queue.id)`
)

// purgeRules lists every sage time-series and its retention window
// (G7-B12, G4-B26). Order matters only for NOT NULL children, which are
// purged before their parents (verification and change_lease before
// decision), and for references kept by a keep predicate (decision before
// action_queue). Nullable references are ON DELETE SET NULL (schema
// migration), so the other purges cannot block each other.
func purgeRules(cfg *config.Config) []purgeRule {
	r := cfg.Retention
	rules := []purgeRule{
		{table: "snapshots", timeCol: "collected_at", days: r.SnapshotsDays,
			extra: keepSnapshotKeyframe, batch: snapshotBatchSize,
			partitioned: &partition.Snapshots},
		{table: "query_store", timeCol: "captured_at", days: r.QueryStoreDays,
			partitioned: &partition.QueryStore},
		{table: "health_history", timeCol: "recorded_at", days: r.SnapshotsDays},
		{table: "size_history", timeCol: "collected_at", days: r.SnapshotsDays},
		// Resolved findings age from their resolution (perf storage phase:
		// last_seen is refreshed every cycle and may not be indexed).
		{table: "findings", timeCol: "resolved_at", days: r.FindingsDays,
			extra: "AND status = 'resolved'"},
		{table: "briefings", timeCol: "generated_at", days: r.FindingsDays},
		// Remembered what-if rejections age from their last measurement.
		{table: "optimizer_rejection", timeCol: "measured_at", days: r.FindingsDays},
		{table: "tuning_budget_day", timeCol: "updated_at", days: r.FindingsDays},
		{table: "alert_log", timeCol: "sent_at", days: r.ActionsDays},
		{table: "notification_log", timeCol: "sent_at", days: r.ActionsDays},
		// Approval cards age from their follow-up (open cards are closed by the
		// follow-up worker after 14 days).
		{table: "approval_card_deliveries", timeCol: "followed_up_at", days: r.ActionsDays},
		{table: "action_log", timeCol: "executed_at", days: r.ActionsDays, extra: keepActionLog},
		{table: "verification", timeCol: "created_at", days: r.ActionsDays,
			extra: keepVerification},
		{table: "change_lease", timeCol: "acquired_at", days: r.ActionsDays,
			extra: "AND state <> 'active'"},
		// A request still waiting for its turn is kept, however old.
		{table: "lease_queue", timeCol: "enqueued_at", days: r.ActionsDays,
			extra: "AND state <> 'waiting'"},
		// Index replacements (roadmap 2.3): a step to resume or a watch in
		// progress is kept, however old.
		{table: "index_replace", timeCol: "updated_at", days: r.ActionsDays,
			extra: "AND state IN ('completed', 'create_failed', 'drop_failed', " +
				"'rolled_back', 'rollback_failed', 'old_restored') " +
				"AND verify_phase IN ('none', 'done')"},
		{table: "decision", timeCol: "created_at", days: r.ActionsDays, extra: keepDecision},
		// Withheld verdicts age from when they were last seen (perf audit F1).
		{table: "decision", timeCol: "COALESCE(last_seen_at, created_at)",
			days: r.DecisionsDays, extra: keepWithheldDecision, sweepCol: "created_at"},
		{table: "action_queue", timeCol: "proposed_at", days: r.ActionsDays,
			extra: keepActionQueue},
		{table: "recommendation", timeCol: "updated_at", days: r.ActionsDays,
			extra: keepRecommendation},
		{table: "retention_run", timeCol: "created_at", days: r.ActionsDays},
		{table: "admission_withheld", timeCol: "last_seen_at", days: r.ActionsDays},
		// Shadow decisions (roadmap 1.4) age out once scored; a pending one
		// is kept so it can still be scored.
		{table: "shadow_decision", timeCol: "recorded_at", days: r.ActionsDays,
			extra: "AND status <> 'pending'"},
		// Binding facts (roadmap 2.3): proposed and confirmed facts are kept;
		// rejected and expired ones age from their last change.
		{table: "facts", timeCol: "updated_at", days: r.ActionsDays,
			extra: "AND status IN ('rejected', 'expired')"},
		{table: "fact_card_deliveries", timeCol: "created_at", days: r.ActionsDays},
		// Source-fix reports (MCP v2): a pending one is kept until its
		// verdict is decided; decided ones age out like actions.
		{table: "source_fix", timeCol: "updated_at", days: r.ActionsDays,
			extra: "AND verdict IS NOT NULL"},
		// Ask Sage conversations age from their last question; answers
		// cascade. The budget's day rows age on the same window.
		{table: "ask_conversations", timeCol: "updated_at", days: cfg.Ask.RetentionDays},
		{table: "ask_budget_day", timeCol: "day", days: cfg.Ask.RetentionDays},
		// Specialist-contract requests (roadmap phase 3): the audit of what
		// external agents asked; a result post still owed is kept.
		{table: "specialist_requests", timeCol: "created_at", days: r.ActionsDays,
			extra: "AND outbound <> 'pending'"},
		// Managed provider changes (roadmap phase 3): open proposals are
		// kept; decided, applied and superseded ones age from their last change.
		{table: "managed_change_proposals", timeCol: "updated_at", days: r.ActionsDays,
			extra: "AND status NOT IN ('pending', 'approved')"},
		{table: "explain_cache", timeCol: "captured_at", days: r.ExplainsDays},
		// A cached explanation is useless once it expires; a day of grace
		// covers a reader racing the expiry. (created_at, the old key, is
		// reset by every refresh of the cache entry.)
		{table: "explain_results", timeCol: "expires_at", days: explainResultsGrace(r)},
		// The sign-in audit trail (E1) is kept a year by default. Swept on
		// created_at so even the generic plan reads it by index (gate A).
		{table: "auth_audit", timeCol: "created_at", days: r.AuthAuditDays,
			sweepCol: "created_at"},
		// Correlated pgaudit records are audit evidence of actions (E2).
		{table: "guard_pgaudit_events", timeCol: "logged_at", days: r.ActionsDays,
			sweepCol: "logged_at"},
		// Used or expired SSO link grants are dead weight once old (D7).
		{table: "user_oidc_link_grants", timeCol: "expires_at", days: r.ActionsDays},
		{table: "sre_eval_runs", timeCol: "ingested_at",
			days: cfg.SRE.Autonomy.ReportRetentionDays, extra: keepEvalRun},
	}
	return rules
}

// explainResultsGrace is one day, or 0 (off) with explain retention off.
func explainResultsGrace(r config.RetentionConfig) int {
	if r.ExplainsDays <= 0 {
		return 0
	}
	return 1
}
