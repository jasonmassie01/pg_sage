package schema

// Agent governance G1 brokered reads (AGENTDB-SPEC §6.8, §7). Idempotent.
//
// sage.guard_query_audit: one row per agent_query call in the database it
// read (every monitored database), with the gate's reason and step. The
// pss_* columns snapshot pg_stat_statements_info when the call ran, so the
// attribution view (G1-10) can tell when evictions or a reset may have
// dropped the principal's statements. Retention: 30 days (§6.17).
const ddlGuardQueryAudit = `
CREATE TABLE IF NOT EXISTS sage.guard_query_audit (
    id            bigserial PRIMARY KEY,
    database_id   uuid NOT NULL,
    principal_id  text NOT NULL CHECK (length(principal_id) BETWEEN 1 AND 100),
    task_id       text CHECK (length(task_id) <= 200),
    envelope_hash text NOT NULL CHECK (length(envelope_hash) BETWEEN 1 AND 128),
    verdict       text NOT NULL CHECK (verdict IN ('execute', 'blocked')),
    reason        text CHECK (length(reason) <= 100),
    step          text CHECK (length(step) <= 10),
    row_count     integer CHECK (row_count >= 0),
    classes       text[] NOT NULL DEFAULT '{}',
    fingerprint   text CHECK (length(fingerprint) <= 64),
    broker_role   text CHECK (length(broker_role) <= 63),
    pss_dealloc   bigint,
    pss_reset     timestamptz,
    at            timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS guard_query_audit_principal_at
    ON sage.guard_query_audit (principal_id, at);
CREATE INDEX IF NOT EXISTS guard_query_audit_at ON sage.guard_query_audit (at);
`
