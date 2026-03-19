/* pg_sage -- upgrade from 0.1.0 to 0.5.0
 *
 * Adds MCP sidecar support: audit log table and JSON helper functions.
 */

-- complain if script is sourced in psql rather than via ALTER EXTENSION
\echo Use "ALTER EXTENSION pg_sage UPDATE TO '0.5.0'" to load this file.\quit

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
-- New SQL functions for MCP sidecar
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
