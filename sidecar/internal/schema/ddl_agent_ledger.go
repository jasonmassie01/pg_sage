package schema

const ddlAgentNativeLedger = `
CREATE TABLE IF NOT EXISTS sage.decision (
    id                  bigserial PRIMARY KEY,
    database_id         bigint,
    feature             text NOT NULL,
    intent              text NOT NULL,
    target_objects      jsonb NOT NULL DEFAULT '[]'::jsonb,
    policy_id           bigint REFERENCES sage.policy(id),
    policy_version      integer,
    verdict             text NOT NULL,
    risk_tier           text NOT NULL,
    reason              text NOT NULL,
    guardrails          jsonb NOT NULL DEFAULT '[]'::jsonb,
    off_window_ok       boolean NOT NULL DEFAULT false,
    deadline_kind       text,
    deadline_hard_at    timestamptz,
    evidence            jsonb NOT NULL DEFAULT '{}'::jsonb,
    evidence_id         text NOT NULL,
    action_log_id       bigint REFERENCES sage.action_log(id),
    queue_id            integer REFERENCES sage.action_queue(id),
    created_at          timestamptz NOT NULL DEFAULT now(),
    resolved_at         timestamptz,
    CHECK (verdict IN (
        'execute', 'queue_approval', 'parked', 'blocked', 'observe_only',
        'recommended'
    )),
    CHECK (risk_tier IN ('read_only', 'safe', 'moderate', 'high'))
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_decision_evidence_id_unique
    ON sage.decision (evidence_id);
CREATE INDEX IF NOT EXISTS idx_decision_open_deadline
    ON sage.decision (deadline_hard_at)
    WHERE resolved_at IS NULL AND deadline_kind IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_decision_database_created
    ON sage.decision (database_id, created_at DESC);

CREATE TABLE IF NOT EXISTS sage.change_lease (
    id              bigserial PRIMARY KEY,
    database_id     bigint,
    object_key      text NOT NULL,
    decision_id     bigint NOT NULL REFERENCES sage.decision(id),
    holder          text NOT NULL,
    intent          text NOT NULL,
    acquired_at     timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    released_at     timestamptz,
    state           text NOT NULL DEFAULT 'active',
    CHECK (state IN ('active', 'released', 'expired'))
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_change_lease_database_object_state_unique
    ON sage.change_lease ((COALESCE(database_id, 0)), object_key)
    WHERE state = 'active';
CREATE INDEX IF NOT EXISTS idx_change_lease_expiry
    ON sage.change_lease (expires_at)
    WHERE state = 'active';

CREATE TABLE IF NOT EXISTS sage.schema_baseline (
    id                              bigserial PRIMARY KEY,
    database_id                     bigint,
    object_identity                 text NOT NULL,
    object_type                     text NOT NULL,
    authorized_definition_hash      text NOT NULL,
    observed_definition_hash        text,
    last_authorized_decision_id     bigint REFERENCES sage.decision(id),
    last_authorized_action_id       bigint REFERENCES sage.action_log(id),
    reconciliation_status           text NOT NULL DEFAULT 'matched',
    observed_at                     timestamptz,
    updated_at                      timestamptz NOT NULL DEFAULT now(),
    CHECK (reconciliation_status IN (
        'matched', 'additive', 'destructive', 'ambiguous', 'reconciling'
    )),
    UNIQUE (database_id, object_identity)
);
CREATE INDEX IF NOT EXISTS idx_schema_baseline_reconciliation
    ON sage.schema_baseline (database_id, reconciliation_status);

ALTER TABLE sage.action_log
    ADD COLUMN IF NOT EXISTS decision_id bigint REFERENCES sage.decision(id);
CREATE INDEX IF NOT EXISTS idx_action_log_decision
    ON sage.action_log (decision_id)
    WHERE decision_id IS NOT NULL;
`
