/* pg_sage -- PostgreSQL advisor extension
 * Install script for version 0.5.0
 *
 * Requires: PostgreSQL 14+
 */

-- complain if script is sourced in psql rather than via CREATE EXTENSION
\echo Use "CREATE EXTENSION pg_sage" to load this file.\quit

-- ---------------------------------------------------------------------------
-- Schema: created automatically by the extension framework (schema = sage
-- in pg_sage.control). Do NOT add CREATE SCHEMA here.
-- ---------------------------------------------------------------------------

-- ---------------------------------------------------------------------------
-- Tables
-- ---------------------------------------------------------------------------

-- Periodic snapshots of database statistics
CREATE TABLE sage.snapshots (
    id              BIGSERIAL PRIMARY KEY,
    collected_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    category        TEXT NOT NULL,  -- 'tables', 'indexes', 'queries', 'system', 'locks', 'sequences', 'replication'
    data            JSONB NOT NULL
);
CREATE INDEX idx_snapshots_time ON sage.snapshots (collected_at DESC);
CREATE INDEX idx_snapshots_category ON sage.snapshots (category, collected_at DESC);

COMMENT ON TABLE sage.snapshots IS 'Periodic snapshots of database statistics collected by pg_sage background workers.';

-- Detected findings / recommendations
CREATE TABLE sage.findings (
    id                  BIGSERIAL PRIMARY KEY,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen           TIMESTAMPTZ NOT NULL DEFAULT now(),
    occurrence_count    INTEGER NOT NULL DEFAULT 1,
    category            TEXT NOT NULL,
    severity            TEXT NOT NULL,
    object_type         TEXT,
    object_identifier   TEXT,
    title               TEXT NOT NULL,
    detail              JSONB NOT NULL,
    recommendation      TEXT,
    recommended_sql     TEXT,
    rollback_sql        TEXT,
    estimated_cost_usd  NUMERIC(10,2),
    status              TEXT NOT NULL DEFAULT 'open',
    suppressed_until    TIMESTAMPTZ,
    resolved_at         TIMESTAMPTZ,
    acted_on_at         TIMESTAMPTZ,
    action_log_id       BIGINT
);
-- Partial unique index: only one open finding per (category, object_identifier)
CREATE UNIQUE INDEX idx_findings_dedup ON sage.findings (category, object_identifier)
    WHERE status = 'open';
CREATE INDEX idx_findings_status ON sage.findings (status, severity, last_seen DESC);
CREATE INDEX idx_findings_object ON sage.findings (object_identifier, category);

COMMENT ON TABLE sage.findings IS 'Findings and recommendations produced by pg_sage analysis routines.';

-- Audit log of automated or manual actions taken
CREATE TABLE sage.action_log (
    id              BIGSERIAL PRIMARY KEY,
    executed_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    action_type     TEXT NOT NULL,
    finding_id      BIGINT REFERENCES sage.findings(id),
    sql_executed    TEXT NOT NULL,
    rollback_sql    TEXT,
    before_state    JSONB,
    after_state     JSONB,
    outcome         TEXT NOT NULL DEFAULT 'pending',
    rollback_reason TEXT,
    measured_at     TIMESTAMPTZ
);
CREATE INDEX idx_action_log_time ON sage.action_log (executed_at DESC);

COMMENT ON TABLE sage.action_log IS 'Audit trail of all actions executed by pg_sage, with rollback information.';

-- Now add the FK from findings.action_log_id -> action_log.id
ALTER TABLE sage.findings
    ADD CONSTRAINT fk_findings_action_log
    FOREIGN KEY (action_log_id) REFERENCES sage.action_log(id);

-- Cache of EXPLAIN plans for tracked queries
CREATE TABLE sage.explain_cache (
    id              BIGSERIAL PRIMARY KEY,
    captured_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    queryid         BIGINT NOT NULL,
    query_text      TEXT,
    plan_json       JSONB NOT NULL,
    source          TEXT NOT NULL,
    total_cost      FLOAT,
    execution_time  FLOAT
);
CREATE INDEX idx_explain_queryid ON sage.explain_cache (queryid, captured_at DESC);

COMMENT ON TABLE sage.explain_cache IS 'Cached EXPLAIN plans captured automatically or on demand for analysis.';

-- Periodic briefing reports
CREATE TABLE sage.briefings (
    id              BIGSERIAL PRIMARY KEY,
    generated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    period_start    TIMESTAMPTZ NOT NULL,
    period_end      TIMESTAMPTZ NOT NULL,
    mode            TEXT NOT NULL,
    content_text    TEXT NOT NULL,
    content_json    JSONB NOT NULL,
    llm_used        BOOLEAN NOT NULL DEFAULT false,
    token_count     INTEGER,
    delivery_status JSONB
);

COMMENT ON TABLE sage.briefings IS 'Generated briefing reports summarising database health over a given period.';

-- Key/value configuration store
CREATE TABLE sage.config (
    key             TEXT PRIMARY KEY,
    value           TEXT NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by      TEXT
);

COMMENT ON TABLE sage.config IS 'Runtime configuration for pg_sage. Changes take effect on next worker cycle.';

-- ---------------------------------------------------------------------------
-- SQL-callable C functions
-- ---------------------------------------------------------------------------

CREATE FUNCTION sage.status()
    RETURNS JSONB
    AS 'pg_sage', 'sage_status'
    LANGUAGE C STRICT VOLATILE;

COMMENT ON FUNCTION sage.status() IS 'Returns the current operational state of pg_sage as a JSONB document.';

CREATE FUNCTION sage.emergency_stop()
    RETURNS boolean
    AS 'pg_sage', 'sage_emergency_stop'
    LANGUAGE C STRICT VOLATILE;

COMMENT ON FUNCTION sage.emergency_stop() IS 'Immediately halts all autonomous actions performed by pg_sage.';

CREATE FUNCTION sage.resume()
    RETURNS boolean
    AS 'pg_sage', 'sage_resume'
    LANGUAGE C STRICT VOLATILE;

COMMENT ON FUNCTION sage.resume() IS 'Resumes autonomous operations after an emergency stop.';

CREATE FUNCTION sage.explain(queryid BIGINT)
    RETURNS TEXT
    AS 'pg_sage', 'sage_explain'
    LANGUAGE C STRICT VOLATILE;

COMMENT ON FUNCTION sage.explain(BIGINT) IS 'Captures and returns the EXPLAIN plan for the query identified by pg_stat_statements queryid.';

CREATE FUNCTION sage.suppress(
    finding_id      BIGINT,
    reason          TEXT DEFAULT '',
    duration_days   INTEGER DEFAULT 30
)
    RETURNS void
    AS 'pg_sage', 'sage_suppress'
    LANGUAGE C VOLATILE;

COMMENT ON FUNCTION sage.suppress(BIGINT, TEXT, INTEGER) IS 'Suppresses a finding for the given number of days with an optional reason.';

CREATE FUNCTION sage.diagnose(question TEXT)
    RETURNS TEXT
    AS 'pg_sage', 'sage_diagnose'
    LANGUAGE C STRICT VOLATILE;

COMMENT ON FUNCTION sage.diagnose(TEXT) IS 'Runs a diagnostic analysis in response to a natural-language question.';

CREATE FUNCTION sage.briefing()
    RETURNS TEXT
    AS 'pg_sage', 'sage_briefing'
    LANGUAGE C STRICT VOLATILE;

COMMENT ON FUNCTION sage.briefing() IS 'Generates and returns an on-demand briefing of current database health.';

-- ---------------------------------------------------------------------------
-- Default configuration
-- ---------------------------------------------------------------------------

INSERT INTO sage.config (key, value, updated_by) VALUES
    ('snapshot_interval_seconds',   '300',      'install'),
    ('analysis_interval_seconds',   '600',      'install'),
    ('briefing_schedule',           '0 8 * * *','install'),
    ('retention_days',              '90',       'install'),
    ('autonomous_mode',             'recommend','install'),  -- 'off', 'recommend', 'auto'
    ('severity_threshold',          'medium',   'install'),  -- minimum severity for autonomous action
    ('llm_endpoint',                '',         'install'),
    ('llm_api_key',                 '',         'install'),
    ('llm_model',                   '',         'install'),
    ('email_recipients',            '',         'install'),
    ('slack_webhook_url',           '',         'install'),
    ('emergency_stop',              'false',    'install'),
    ('cost_per_cpu_hour_usd',       '0.05',    'install'),
    ('cost_per_iops',               '0.000001','install'),
    ('explain_auto_capture',        'true',     'install'),
    ('explain_min_duration_ms',     '1000',     'install');

-- ---------------------------------------------------------------------------
-- MCP audit log table
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS sage.mcp_log (
    id              BIGSERIAL PRIMARY KEY,
    logged_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    client_ip       TEXT NOT NULL,
    method          TEXT NOT NULL,
    resource_uri    TEXT,
    tool_name       TEXT,
    tokens_used     INTEGER DEFAULT 0,
    duration_ms     DOUBLE PRECISION,
    status          TEXT NOT NULL DEFAULT 'ok',
    error_message   TEXT
);
CREATE INDEX IF NOT EXISTS idx_mcp_log_time ON sage.mcp_log (logged_at DESC);

COMMENT ON TABLE sage.mcp_log IS 'Audit log of MCP sidecar requests for observability and rate-limiting.';

-- ---------------------------------------------------------------------------
-- MCP sidecar JSON helper functions
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION sage.health_json()
    RETURNS jsonb
    LANGUAGE c STABLE
    AS 'pg_sage', 'sage_health_json';

COMMENT ON FUNCTION sage.health_json() IS 'Returns system health overview as JSONB for MCP sidecar consumption.';

CREATE OR REPLACE FUNCTION sage.findings_json(status_filter TEXT DEFAULT 'open')
    RETURNS jsonb
    LANGUAGE c STABLE
    AS 'pg_sage', 'sage_findings_json';

COMMENT ON FUNCTION sage.findings_json(TEXT) IS 'Returns findings array as JSONB, filtered by status.';

CREATE OR REPLACE FUNCTION sage.schema_json(table_name TEXT)
    RETURNS jsonb
    LANGUAGE c STABLE
    AS 'pg_sage', 'sage_schema_json';

COMMENT ON FUNCTION sage.schema_json(TEXT) IS 'Returns DDL, indexes, constraints, columns, and foreign keys for a table as JSONB.';

CREATE OR REPLACE FUNCTION sage.stats_json(table_name TEXT)
    RETURNS jsonb
    LANGUAGE c STABLE
    AS 'pg_sage', 'sage_stats_json';

COMMENT ON FUNCTION sage.stats_json(TEXT) IS 'Returns table size, row counts, dead tuples, index usage, and vacuum status as JSONB.';

CREATE OR REPLACE FUNCTION sage.slow_queries_json()
    RETURNS jsonb
    LANGUAGE c STABLE
    AS 'pg_sage', 'sage_slow_queries_json';

COMMENT ON FUNCTION sage.slow_queries_json() IS 'Returns top slow queries from pg_stat_statements as JSONB array.';

CREATE OR REPLACE FUNCTION sage.explain_json(qid BIGINT)
    RETURNS jsonb
    LANGUAGE c STABLE
    AS 'pg_sage', 'sage_explain_json';

COMMENT ON FUNCTION sage.explain_json(BIGINT) IS 'Returns cached explain plan from sage.explain_cache as JSONB, or null.';
