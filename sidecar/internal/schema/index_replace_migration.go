package schema

// sage.index_replace is the durable state machine of the executor's replace
// action (roadmap 2.3): create the wider index CONCURRENTLY, check it is
// valid, then drop the subsumed index CONCURRENTLY. CONCURRENTLY cannot
// run in a transaction, so each step is recorded before it runs and a
// restart resumes or rolls back from the recorded step and the catalog.
// The old index's OID and definition are kept for the identity check
// before the drop and for the soft drop (re-created on regression).
// action_log_id is not a foreign key: REFERENCES would lock the busy
// sage.action_log on every start. Idempotent.
const ddlIndexReplace = `
CREATE TABLE IF NOT EXISTS sage.index_replace (
    id              bigserial PRIMARY KEY,
    database_id     bigint,
    finding_id      bigint,
    action_log_id   bigint,
    decision_id     bigint,
    approved_by     integer,
    table_name      text NOT NULL,
    new_index       text NOT NULL,
    new_index_oid   bigint,
    create_sql      text NOT NULL,
    drop_sql        text NOT NULL,
    old_index       text NOT NULL,
    old_index_oid   bigint NOT NULL,
    old_definition  text NOT NULL,
    rollback_sql    text NOT NULL,
    state           text NOT NULL,
    verify_phase    text NOT NULL DEFAULT 'none',
    before_state    jsonb NOT NULL DEFAULT '{}'::jsonb,
    error           text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT index_replace_state_check CHECK (state IN ('creating', 'created',
        'dropping', 'completed', 'create_failed', 'drop_failed',
        'rollback_recreating', 'rollback_dropping', 'rolled_back', 'rollback_failed',
        'old_restoring', 'old_restored')),
    CONSTRAINT index_replace_phase_check CHECK (verify_phase IN ('none', 'judging',
        'watch', 'done'))
);
CREATE INDEX IF NOT EXISTS idx_index_replace_open
    ON sage.index_replace (id)
    WHERE state IN ('creating', 'created', 'dropping', 'rollback_recreating',
        'rollback_dropping', 'old_restoring')
       OR verify_phase IN ('judging', 'watch');
CREATE INDEX IF NOT EXISTS idx_index_replace_action
    ON sage.index_replace (action_log_id)
    WHERE action_log_id IS NOT NULL;
`
