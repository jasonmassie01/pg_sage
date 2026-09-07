package schema

const ddlAgentNativeValue = `
CREATE TABLE IF NOT EXISTS sage.toil_model (
    action_type     text NOT NULL,
    model_version   integer NOT NULL,
    base_minutes    numeric NOT NULL,
    notes           text,
    provenance      text NOT NULL DEFAULT 'pg_sage_seed',
    effective_from  timestamptz NOT NULL DEFAULT now(),
    effective_to    timestamptz,
    PRIMARY KEY (action_type, model_version),
    CHECK (base_minutes >= 0)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_toil_model_action_type_model_version_unique
    ON sage.toil_model (action_type, model_version);
CREATE UNIQUE INDEX IF NOT EXISTS idx_toil_model_active_action
    ON sage.toil_model (action_type)
    WHERE effective_to IS NULL;

INSERT INTO sage.toil_model (
    action_type, model_version, base_minutes, notes, provenance
) VALUES
    ('analyze_table', 1, 15, 'Analyze and validate table statistics', 'phase0'),
    ('create_index_concurrently', 1, 45, 'Analyze, build, and verify index', 'phase0'),
    ('drop_unused_index', 1, 20, 'Prove disuse, remove, and verify', 'phase0'),
    ('reindex_concurrently', 1, 30, 'Plan, rebuild, and verify index', 'phase0'),
    ('vacuum_table', 1, 20, 'Diagnose and vacuum table', 'phase0'),
    ('freeze_table', 1, 20, 'Diagnose and freeze table', 'phase0'),
    ('set_table_autovacuum', 1, 30, 'Tune and verify table autovacuum', 'phase0'),
    ('alter_system_guc', 1, 25, 'Assess, apply, and verify configuration', 'phase0'),
    ('create_statistics', 1, 30, 'Analyze and create extended statistics', 'phase0'),
    ('apply_query_hint', 1, 25, 'Assess, apply, and verify query hint', 'phase0'),
    ('online_migration', 1, 120, 'Plan, rehearse, apply, and verify migration', 'phase0'),
    ('fk_supporting_index', 1, 30, 'Detect and build foreign-key index', 'phase0'),
    ('retention_policy_setup', 1, 60, 'Design and verify retention policy', 'phase0'),
    ('slot_bound', 1, 30, 'Diagnose and bound retained WAL', 'phase0'),
    ('slot_drop', 1, 30, 'Prove abandonment and remove slot', 'phase0')
ON CONFLICT (action_type, model_version) DO NOTHING;

ALTER TABLE sage.action_log
    ADD COLUMN IF NOT EXISTS database_id bigint,
    ADD COLUMN IF NOT EXISTS toil_minutes_saved numeric,
    ADD COLUMN IF NOT EXISTS toil_model_version integer;

CREATE TABLE IF NOT EXISTS sage.incident_avoided (
    id                  bigserial PRIMARY KEY,
    database_id         bigint,
    kind                text NOT NULL,
    severity            text NOT NULL,
    credited_minutes    numeric NOT NULL,
    evidence_id         text NOT NULL,
    model_version       integer NOT NULL DEFAULT 1,
    decision_id         bigint NOT NULL REFERENCES sage.decision(id),
    action_log_id       bigint NOT NULL REFERENCES sage.action_log(id),
    verification_id     bigint NOT NULL REFERENCES sage.verification(id),
    occurred_at         timestamptz NOT NULL DEFAULT now(),
    CHECK (kind IN ('xid_wraparound', 'disk_full_slot', 'lock_storm')),
    CHECK (severity IN ('near_miss', 'prevented')),
    CHECK (credited_minutes >= 0)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_incident_avoided_evidence_unique
    ON sage.incident_avoided (evidence_id);
CREATE INDEX IF NOT EXISTS idx_incident_avoided_database_time
    ON sage.incident_avoided (database_id, occurred_at DESC);

CREATE OR REPLACE VIEW sage.value_rollup AS
SELECT
    date_trunc('day', al.executed_at) AS day,
    al.database_id,
    al.action_type,
    count(*) FILTER (
        WHERE al.outcome = 'success' AND al.toil_minutes_saved IS NOT NULL
    )::bigint AS actions_verified,
    COALESCE(sum(al.toil_minutes_saved) FILTER (
        WHERE al.outcome = 'success'
    ), 0::numeric) AS toil_minutes_saved,
    count(*) FILTER (
        WHERE al.outcome IN ('reverted', 'rolled_back')
    )::bigint AS actions_reverted,
    0::numeric AS potential_minutes_pending,
    0::bigint AS incidents_avoided,
    0::numeric AS incident_minutes_credited
FROM sage.action_log al
GROUP BY 1, 2, 3
UNION ALL
SELECT
    date_trunc('day', aq.proposed_at) AS day,
    aq.database_id::bigint AS database_id,
    COALESCE(aq.action_type, 'unknown') AS action_type,
    0::bigint AS actions_verified,
    0::numeric AS toil_minutes_saved,
    0::bigint AS actions_reverted,
    COALESCE(sum(tm.base_minutes), 0::numeric) AS potential_minutes_pending,
    0::bigint AS incidents_avoided,
    0::numeric AS incident_minutes_credited
FROM sage.action_queue aq
LEFT JOIN sage.toil_model tm
  ON tm.action_type = aq.action_type AND tm.effective_to IS NULL
WHERE aq.status = 'pending'
GROUP BY 1, 2, 3
UNION ALL
SELECT
    date_trunc('day', ia.occurred_at) AS day,
    ia.database_id,
    'incident:' || ia.kind AS action_type,
    0::bigint AS actions_verified,
    0::numeric AS toil_minutes_saved,
    0::bigint AS actions_reverted,
    0::numeric AS potential_minutes_pending,
    count(*)::bigint AS incidents_avoided,
    COALESCE(sum(ia.credited_minutes), 0::numeric) AS incident_minutes_credited
FROM sage.incident_avoided ia
GROUP BY 1, 2, 3;
`
