package schema

// Performance gate (reviews/2026-10-03-perf-gate-report.md): pg_sage must
// never need a DBA's index. TestPerfGate found the retention purges of
// explain_cache, alert_log, verification and resolved findings, the change
// feed's age-out and the foreign-key actions fired by purging action_log,
// decision, verification and action_queue reading whole history tables.
// These indexes make each of them an index range or lookup. Idempotent.
//
// Plain CREATE INDEX (not CONCURRENTLY: bootstrap runs in one session under
// its advisory lock) briefly blocks writes to the table while it builds;
// these tables are pg_sage's own and the build runs once, at startup.
const ddlPerfIndexes = `
CREATE INDEX IF NOT EXISTS idx_explain_cache_captured
    ON sage.explain_cache (captured_at);
CREATE INDEX IF NOT EXISTS idx_alert_log_sent
    ON sage.alert_log (sent_at);
CREATE INDEX IF NOT EXISTS idx_verification_created
    ON sage.verification (created_at);
CREATE INDEX IF NOT EXISTS idx_verification_decision
    ON sage.verification (decision_id);
CREATE INDEX IF NOT EXISTS idx_findings_resolved_last_seen
    ON sage.findings (last_seen) WHERE status = 'resolved';
CREATE INDEX IF NOT EXISTS idx_sre_change_events_received
    ON sage.sre_change_events (deployment_id, received_at);
CREATE INDEX IF NOT EXISTS idx_findings_action_log
    ON sage.findings (action_log_id) WHERE action_log_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_decision_action_log
    ON sage.decision (action_log_id) WHERE action_log_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_decision_queue
    ON sage.decision (queue_id) WHERE queue_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_action_queue_action_log
    ON sage.action_queue (action_log_id) WHERE action_log_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_change_lease_decision
    ON sage.change_lease (decision_id);
CREATE INDEX IF NOT EXISTS idx_incident_avoided_action_log
    ON sage.incident_avoided (action_log_id);
CREATE INDEX IF NOT EXISTS idx_incident_avoided_decision
    ON sage.incident_avoided (decision_id);
CREATE INDEX IF NOT EXISTS idx_incident_avoided_verify
    ON sage.incident_avoided (verification_id);
CREATE INDEX IF NOT EXISTS idx_schema_baseline_action
    ON sage.schema_baseline (last_authorized_action_id)
    WHERE last_authorized_action_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_schema_baseline_decision
    ON sage.schema_baseline (last_authorized_decision_id)
    WHERE last_authorized_decision_id IS NOT NULL;
`
