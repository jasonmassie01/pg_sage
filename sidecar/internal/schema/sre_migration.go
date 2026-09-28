package schema

// Sage SRE M1 coordination state (AI-SRE-SPEC §5/§10, Codex contracts
// §2): deployment and database identity, durable investigations with
// lease/fence columns, idempotent steps, immutable evidence and durable
// model-budget reservations. The CHECK limits are the R1 hard ceilings;
// configuration may tighten, never raise, them. Bootstrap installs the
// tables in the meta database when one is configured and in the
// monitored database otherwise. Additive and idempotent.
const ddlSRECoordination = `
CREATE TABLE IF NOT EXISTS sage.sre_deployments (
    singleton     boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    deployment_id uuid NOT NULL UNIQUE,
    created_at    timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE IF NOT EXISTS sage.sre_database_bindings (
    deployment_id      uuid NOT NULL,
    database_id        uuid NOT NULL,
    runtime_key        text NOT NULL CHECK (length(runtime_key) BETWEEN 1 AND 128),
    legacy_database_id integer,
    identity_strength  text NOT NULL
        CHECK (identity_strength IN ('configured', 'provider', 'cluster')),
    cluster_epoch      text NOT NULL CHECK (length(cluster_epoch) BETWEEN 1 AND 128),
    created_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (deployment_id, database_id),
    UNIQUE (deployment_id, runtime_key)
);

CREATE TABLE IF NOT EXISTS sage.sre_investigations (
    deployment_id       uuid NOT NULL,
    database_id         uuid NOT NULL,
    id                  uuid NOT NULL,
    source_case_id      text NOT NULL CHECK (length(source_case_id) BETWEEN 1 AND 256),
    trigger_kind        text NOT NULL,
    trigger_fingerprint bytea NOT NULL CHECK (octet_length(trigger_fingerprint) = 32),
    idempotency_key     text CHECK (length(idempotency_key) BETWEEN 1 AND 160),
    state               text NOT NULL CHECK (state IN (
        'queued', 'collecting', 'evaluating', 'needs_evidence', 'concluded',
        'inconclusive', 'paused', 'cancelled', 'expired', 'failed')),
    version             bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    fence_token         bigint NOT NULL DEFAULT 0 CHECK (fence_token >= 0),
    lease_owner         uuid,
    lease_started_at    timestamptz,
    lease_until         timestamptz,
    segment_deadline    timestamptz,
    active_ms           bigint NOT NULL DEFAULT 0 CHECK (active_ms BETWEEN 0 AND 120000),
    probe_count         integer NOT NULL DEFAULT 0 CHECK (probe_count BETWEEN 0 AND 12),
    model_turns         integer NOT NULL DEFAULT 0 CHECK (model_turns BETWEEN 0 AND 2),
    created_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at          timestamptz NOT NULL,
    failure_code        text,
    PRIMARY KEY (deployment_id, database_id, id),
    FOREIGN KEY (deployment_id, database_id)
        REFERENCES sage.sre_database_bindings (deployment_id, database_id),
    CHECK ((lease_owner IS NULL) = (lease_until IS NULL)),
    CHECK ((lease_owner IS NULL) = (lease_started_at IS NULL)),
    CHECK (expires_at > created_at)
);
CREATE UNIQUE INDEX IF NOT EXISTS sre_one_live_trigger
    ON sage.sre_investigations (deployment_id, database_id, trigger_fingerprint)
    WHERE state IN ('queued', 'collecting', 'evaluating', 'needs_evidence', 'paused');
CREATE UNIQUE INDEX IF NOT EXISTS sre_investigation_idempotency
    ON sage.sre_investigations (deployment_id, database_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS sre_investigation_queue
    ON sage.sre_investigations (deployment_id, database_id, state, updated_at);

CREATE TABLE IF NOT EXISTS sage.sre_steps (
    deployment_id    uuid NOT NULL,
    database_id      uuid NOT NULL,
    investigation_id uuid NOT NULL,
    id               uuid NOT NULL,
    sequence         integer NOT NULL CHECK (sequence > 0),
    idempotency_key  text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 160),
    fence_token      bigint NOT NULL CHECK (fence_token > 0),
    probe_count      integer NOT NULL CHECK (probe_count >= 0),
    next_state       text NOT NULL,
    error_code       text,
    created_at       timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (deployment_id, database_id, investigation_id, id),
    UNIQUE (deployment_id, database_id, investigation_id, idempotency_key),
    UNIQUE (deployment_id, database_id, investigation_id, sequence),
    FOREIGN KEY (deployment_id, database_id, investigation_id)
        REFERENCES sage.sre_investigations (deployment_id, database_id, id)
);

CREATE TABLE IF NOT EXISTS sage.sre_evidence (
    deployment_id    uuid NOT NULL,
    database_id      uuid NOT NULL,
    investigation_id uuid NOT NULL,
    id               uuid NOT NULL,
    step_id          uuid NOT NULL,
    source_kind      text NOT NULL,
    probe_id         text NOT NULL CHECK (length(probe_id) BETWEEN 1 AND 64),
    probe_version    text NOT NULL,
    observed_at      timestamptz,
    collected_at     timestamptz NOT NULL DEFAULT clock_timestamp(),
    capability_state text NOT NULL CHECK (capability_state IN (
        'available', 'unsupported', 'permission_denied', 'unreachable', 'unknown')),
    reason_code      text,
    payload_version  integer NOT NULL CHECK (payload_version > 0),
    classification   text NOT NULL CHECK (classification IN ('redacted', 'restricted')),
    payload          jsonb NOT NULL,
    sha256           bytea NOT NULL CHECK (octet_length(sha256) = 32),
    PRIMARY KEY (deployment_id, database_id, investigation_id, id),
    FOREIGN KEY (deployment_id, database_id, investigation_id, step_id)
        REFERENCES sage.sre_steps (deployment_id, database_id, investigation_id, id),
    CHECK (jsonb_typeof(payload) = 'object'),
    CHECK (octet_length(payload::text) <= 262144),
    CHECK (capability_state = 'available' OR reason_code IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS sre_evidence_time
    ON sage.sre_evidence (deployment_id, database_id, investigation_id, observed_at);

CREATE TABLE IF NOT EXISTS sage.sre_budget_reservations (
    deployment_id    uuid NOT NULL,
    database_id      uuid NOT NULL,
    id               uuid NOT NULL,
    investigation_id uuid,
    utc_day          date NOT NULL,
    caller_kind      text NOT NULL,
    request_key      text NOT NULL CHECK (length(request_key) BETWEEN 1 AND 160),
    state            text NOT NULL CHECK (state IN
        ('reserved', 'inflight', 'settled', 'unknown', 'cancelled')),
    input_reserved   bigint NOT NULL CHECK (input_reserved >= 0),
    output_reserved  bigint NOT NULL CHECK (output_reserved >= 0),
    input_used       bigint CHECK (input_used >= 0),
    output_used      bigint CHECK (output_used >= 0),
    version          bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at       timestamptz NOT NULL DEFAULT clock_timestamp(),
    settled_at       timestamptz,
    PRIMARY KEY (deployment_id, database_id, id),
    FOREIGN KEY (deployment_id, database_id)
        REFERENCES sage.sre_database_bindings (deployment_id, database_id),
    FOREIGN KEY (deployment_id, database_id, investigation_id)
        REFERENCES sage.sre_investigations (deployment_id, database_id, id),
    CHECK ((input_used IS NULL) = (output_used IS NULL)),
    CHECK ((state = 'settled') = (input_used IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS sre_budget_request
    ON sage.sre_budget_reservations (deployment_id, database_id,
        COALESCE(investigation_id, '00000000-0000-0000-0000-000000000000'::uuid),
        request_key);
CREATE INDEX IF NOT EXISTS sre_budget_day
    ON sage.sre_budget_reservations (deployment_id, utc_day, database_id);
`
