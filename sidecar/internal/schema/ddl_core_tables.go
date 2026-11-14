package schema

// config-31: the base-table DDL, split out of bootstrap.go. A fresh install
// runs fullSchemaDDL; ensureTablesExist creates a missing table from
// expectedTables. testdata/ddl_constants.sha256 pins every value: a
// changed byte is a schema change, so it belongs in a new migration step.

const fullSchemaDDL = `
CREATE SCHEMA IF NOT EXISTS sage;
` + ddlActionLog + ddlSnapshots + ddlFindings +
	ddlExplainCache + ddlBriefings + ddlConfig +
	ddlAlertLog + ddlQueryHints + ddlExplainSourceIdx +
	ddlUsers + ddlSessions + ddlDatabases +
	ddlNotificationChannels + ddlNotificationRules +
	ddlNotificationLog + ddlActionQueue +
	ddlActionLogApprovalCols + ddlUsersOAuth +
	ddlQueryHintsRewrite + ddlQueryHintsRevalidate +
	ddlIncidents + ddlExplainResults +
	ddlSchemaFindings + ddlCryptoMeta + ddlHealthHistory +
	ddlFleetScaleIndexes + ddlQueryStore + ddlRecommendation +
	ddlRecommendationRevision + ddlRecommendationTransition

const ddlActionLog = `
CREATE TABLE IF NOT EXISTS sage.action_log (
    id              bigserial PRIMARY KEY,
    executed_at     timestamptz NOT NULL DEFAULT now(),
    action_type     text NOT NULL,
    finding_id      bigint,
    sql_executed    text NOT NULL,
    rollback_sql    text,
    before_state    jsonb,
    after_state     jsonb,
    outcome         text NOT NULL DEFAULT 'pending',
    rollback_reason text,
    measured_at     timestamptz
);
CREATE INDEX IF NOT EXISTS idx_action_log_time
    ON sage.action_log (executed_at DESC);
CREATE INDEX IF NOT EXISTS idx_action_log_finding
    ON sage.action_log (finding_id)
    WHERE finding_id IS NOT NULL;
`

const ddlSnapshots = `
CREATE TABLE IF NOT EXISTS sage.snapshots (
    id              bigserial PRIMARY KEY,
    collected_at    timestamptz NOT NULL DEFAULT now(),
    category        text NOT NULL,
    data            jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_snapshots_time
    ON sage.snapshots (collected_at DESC);
CREATE INDEX IF NOT EXISTS idx_snapshots_category
    ON sage.snapshots (category, collected_at DESC);
`

const ddlFindings = `
CREATE TABLE IF NOT EXISTS sage.findings (
    id                  bigserial PRIMARY KEY,
    created_at          timestamptz NOT NULL DEFAULT now(),
    last_seen           timestamptz NOT NULL DEFAULT now(),
    occurrence_count    integer NOT NULL DEFAULT 1,
    category            text NOT NULL,
    severity            text NOT NULL,
    object_type         text,
    object_identifier   text,
    title               text NOT NULL,
    detail              jsonb NOT NULL,
    recommendation      text,
    recommended_sql     text,
    rollback_sql        text,
    estimated_cost_usd  numeric(10,2),
    status              text NOT NULL DEFAULT 'open',
    suppressed_until    timestamptz,
    resolved_at         timestamptz,
    acted_on_at         timestamptz,
    action_log_id       bigint REFERENCES sage.action_log(id),
    -- v0.11 — absorbs sage.schema_findings. rule_id and impact_score are
    -- populated only for findings with category = 'schema_lint:<rule>'.
    rule_id             text,
    impact_score        real
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_findings_dedup
    ON sage.findings (category, object_identifier)
    WHERE status = 'open';
CREATE INDEX IF NOT EXISTS idx_findings_object
    ON sage.findings (object_identifier, category);
CREATE INDEX IF NOT EXISTS idx_findings_category_status
    ON sage.findings (category, severity)
    WHERE status = 'open';
`

const ddlExplainCache = `
CREATE TABLE IF NOT EXISTS sage.explain_cache (
    id              bigserial PRIMARY KEY,
    captured_at     timestamptz NOT NULL DEFAULT now(),
    queryid         bigint NOT NULL,
    query_text      text,
    plan_json       jsonb NOT NULL,
    source          text NOT NULL,
    total_cost      float,
    execution_time  float
);
CREATE INDEX IF NOT EXISTS idx_explain_queryid
    ON sage.explain_cache (queryid, captured_at DESC);
`

const ddlBriefings = `
CREATE TABLE IF NOT EXISTS sage.briefings (
    id              bigserial PRIMARY KEY,
    generated_at    timestamptz NOT NULL DEFAULT now(),
    period_start    timestamptz NOT NULL,
    period_end      timestamptz NOT NULL,
    mode            text NOT NULL,
    content_text    text NOT NULL,
    content_json    jsonb NOT NULL,
    llm_used        boolean NOT NULL DEFAULT false,
    token_count     integer,
    delivery_status jsonb
);
`

const ddlCryptoMeta = `
CREATE TABLE IF NOT EXISTS sage.crypto_meta (
    id          integer PRIMARY KEY DEFAULT 1,
    kdf_salt    bytea NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT crypto_meta_singleton CHECK (id = 1)
);
`

const ddlConfig = `
CREATE TABLE IF NOT EXISTS sage.config (
    key             text PRIMARY KEY,
    value           text NOT NULL,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    updated_by      text
);
`

const ddlAlertLog = `
CREATE TABLE IF NOT EXISTS sage.alert_log (
    id            bigserial PRIMARY KEY,
    sent_at       timestamptz NOT NULL DEFAULT now(),
    finding_id    bigint REFERENCES sage.findings(id),
    severity      text NOT NULL,
    channel       text NOT NULL,
    dedup_key     text NOT NULL,
    status        text NOT NULL DEFAULT 'sent',
    error_message text
);
CREATE INDEX IF NOT EXISTS idx_alert_log_dedup
    ON sage.alert_log (dedup_key, sent_at DESC);
CREATE INDEX IF NOT EXISTS idx_alert_log_finding
    ON sage.alert_log (finding_id);
`

const ddlQueryHints = `
CREATE TABLE IF NOT EXISTS sage.query_hints (
    id             bigserial PRIMARY KEY,
    created_at     timestamptz NOT NULL DEFAULT now(),
    queryid        bigint NOT NULL,
    hint_plan_id   bigint,
    hint_text      text NOT NULL,
    symptom        text NOT NULL,
    before_cost    float,
    after_cost     float,
    status         text NOT NULL DEFAULT 'active',
    verified_at    timestamptz,
    rolled_back_at timestamptz
);
CREATE INDEX IF NOT EXISTS idx_query_hints_queryid
    ON sage.query_hints (queryid) WHERE status = 'active';
`

const ddlExplainSourceIdx = `
CREATE INDEX IF NOT EXISTS idx_explain_source
    ON sage.explain_cache (source, queryid, captured_at DESC);
`

const ddlUsers = `
CREATE TABLE IF NOT EXISTS sage.users (
    id          SERIAL PRIMARY KEY,
    email       TEXT UNIQUE NOT NULL,
    password    TEXT NOT NULL,
    role        TEXT NOT NULL DEFAULT 'viewer',
    created_at  TIMESTAMPTZ DEFAULT now(),
    last_login  TIMESTAMPTZ
);
`

const ddlSessions = `
CREATE TABLE IF NOT EXISTS sage.sessions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     INT REFERENCES sage.users(id) ON DELETE CASCADE,
    expires_at  TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_sessions_expires
    ON sage.sessions (expires_at);
CREATE INDEX IF NOT EXISTS idx_sessions_user
    ON sage.sessions (user_id);
`

const ddlActionQueue = `
CREATE TABLE IF NOT EXISTS sage.action_queue (
    id              SERIAL PRIMARY KEY,
    database_id     INT,
    finding_id      INT,
    proposed_sql    TEXT NOT NULL,
    rollback_sql    TEXT,
    action_risk     TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending',
    proposed_at     TIMESTAMPTZ DEFAULT now(),
    decided_by      INT,
    decided_at      TIMESTAMPTZ,
    expires_at      TIMESTAMPTZ DEFAULT now() + INTERVAL '7 days',
    reason          TEXT,
    action_type     TEXT,
    identity_key    TEXT,
    policy_decision TEXT,
    guardrails      JSONB NOT NULL DEFAULT '[]'::jsonb,
    attempt_count   INT NOT NULL DEFAULT 0,
    last_attempt_at TIMESTAMPTZ,
    cooldown_until  TIMESTAMPTZ,
    failure_fingerprint TEXT,
    last_failure_fingerprint TEXT,
    verification_status TEXT NOT NULL DEFAULT 'not_started',
    shadow_toil_minutes INT NOT NULL DEFAULT 0,
    action_log_id  BIGINT REFERENCES sage.action_log(id)
);
CREATE INDEX IF NOT EXISTS idx_action_queue_status
    ON sage.action_queue (status, proposed_at DESC);
CREATE INDEX IF NOT EXISTS idx_action_queue_finding
    ON sage.action_queue (finding_id)
    WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_action_queue_identity
    ON sage.action_queue (identity_key, status)
    WHERE identity_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_action_queue_database_pending
    ON sage.action_queue (database_id, status, proposed_at DESC)
    WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_action_queue_expiry
    ON sage.action_queue (expires_at)
    WHERE status IN ('pending', 'failed');
`

// v0.9 — Root Cause Analysis incidents table.
// v0.9.1 — expanded CHECK constraints for log-based RCA sources,
// info severity, and Tier 2 action_risk values.
const ddlIncidents = `
CREATE TABLE IF NOT EXISTS sage.incidents (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    detected_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_detected_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    severity          TEXT NOT NULL CHECK (severity IN ('info', 'warning', 'critical')),
    root_cause        TEXT NOT NULL,
    causal_chain      JSONB NOT NULL DEFAULT '[]',
    affected_objects  TEXT[] NOT NULL DEFAULT '{}',
    signal_ids        TEXT[] NOT NULL DEFAULT '{}',
    recommended_sql   TEXT,
    rollback_sql      TEXT,
    action_risk       TEXT CHECK (action_risk IN (
        'safe', 'moderate', 'high_risk', 'low', 'medium', 'high'
    ) OR action_risk IS NULL),
    source            TEXT NOT NULL CHECK (source IN (
        'deterministic', 'log_deterministic',
        'self_action', 'manual_review_required', 'llm',
        'schema_advisor', 'schema_lint', 'n_plus_one'
    )),
    confidence        NUMERIC(3,2) NOT NULL DEFAULT 1.0,
    related_findings  UUID[],
    resolved_at       TIMESTAMPTZ,
    database_name     TEXT,
    occurrence_count  INT NOT NULL DEFAULT 1,
    escalated_at      TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_incidents_active ON sage.incidents (detected_at DESC) WHERE resolved_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_incidents_severity ON sage.incidents (severity) WHERE resolved_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_incidents_database ON sage.incidents (database_name, detected_at DESC);
`

// v0.10 — Schema lint findings table.
const ddlSchemaFindings = `
CREATE TABLE IF NOT EXISTS sage.schema_findings (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    rule_id       TEXT NOT NULL,
    schema_name   TEXT NOT NULL,
    table_name    TEXT NOT NULL,
    column_name   TEXT NOT NULL DEFAULT '',
    index_name    TEXT NOT NULL DEFAULT '',
    severity      TEXT NOT NULL CHECK (severity IN ('info', 'warning', 'critical')),
    category      TEXT NOT NULL CHECK (category IN (
        'safety', 'performance', 'correctness', 'convention',
        'indexing', 'data_integrity', 'schema_design', 'maintenance'
    )),
    description   TEXT NOT NULL,
    impact        TEXT NOT NULL DEFAULT '',
    suggestion    TEXT,
    suggested_sql TEXT,
    table_size    BIGINT,
    impact_score  REAL,
    first_seen    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen     TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at   TIMESTAMPTZ,
    suppressed    BOOLEAN NOT NULL DEFAULT false,
    database_name TEXT,
    query_count   BIGINT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_schema_findings_active_key
    ON sage.schema_findings (
        rule_id, schema_name, table_name, column_name, index_name
    )
    WHERE resolved_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_schema_findings_severity
    ON sage.schema_findings (severity) WHERE resolved_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_schema_findings_table
    ON sage.schema_findings (schema_name, table_name);
`

// v0.9 — Natural Language EXPLAIN cache (replaces the old explain_cache for this purpose).
// The existing explain_cache stores auto_explain plan captures.
// This new table caches LLM-generated explain results with TTL.
const ddlExplainResults = `
CREATE TABLE IF NOT EXISTS sage.explain_results (
    query_hash      BIGINT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ NOT NULL,
    plan_json       JSONB NOT NULL,
    explanation     JSONB NOT NULL,
    database_name   TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (query_hash, database_name)
);
`

// v0.12 — Per-database fleet health time series. Written by the
// fleet manager every time a health score is recomputed (or at a
// periodic tick so charts stay populated even when health is flat).
// Used by GET /api/v1/fleet/health.
const ddlHealthHistory = `
CREATE TABLE IF NOT EXISTS sage.health_history (
    id            BIGSERIAL PRIMARY KEY,
    recorded_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    database_name TEXT NOT NULL,
    health_score  INTEGER NOT NULL,
    findings_open INTEGER NOT NULL DEFAULT 0,
    findings_critical INTEGER NOT NULL DEFAULT 0,
    findings_warning INTEGER NOT NULL DEFAULT 0,
    findings_info INTEGER NOT NULL DEFAULT 0,
    actions_total INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_health_history_lookup
    ON sage.health_history (database_name, recorded_at DESC);
CREATE INDEX IF NOT EXISTS idx_health_history_recent
    ON sage.health_history (recorded_at DESC);
`

// ddlQueryStore is the per-queryid metrics time-series (F2). It records a
// sample per top query each cycle so windowed latency can be computed
// (pg_stat_statements only exposes lifetime averages). It is the
// substrate for per-queryid verify-and-revert (F1) and plan-regression
// detection (A5). Plan_hash is nullable until a plan is captured.
const ddlQueryStore = `
CREATE TABLE IF NOT EXISTS sage.query_store (
    id              bigserial PRIMARY KEY,
    captured_at     timestamptz NOT NULL DEFAULT now(),
    queryid         bigint NOT NULL,
    calls           bigint NOT NULL,
    total_exec_time double precision NOT NULL,
    mean_exec_time  double precision NOT NULL,
    rows            bigint NOT NULL DEFAULT 0,
    plan_hash       text
);
CREATE INDEX IF NOT EXISTS idx_query_store_qid_time
    ON sage.query_store (queryid, captured_at DESC);
CREATE INDEX IF NOT EXISTS idx_query_store_time
    ON sage.query_store (captured_at DESC);
`
