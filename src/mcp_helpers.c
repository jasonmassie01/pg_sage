/*
 * mcp_helpers.c — SQL-callable functions returning pre-formatted JSONB
 *                 for the MCP sidecar to consume.
 *
 * All functions connect via SPI, build a JSON string, and return JSONB.
 * AGPL-3.0 License
 */

#include "pg_sage.h"

#include <string.h>
#include "lib/stringinfo.h"
#include "executor/spi.h"
#include "utils/builtins.h"
#include "utils/jsonb.h"
#include "catalog/pg_type.h"
#include "funcapi.h"

/* ----------------------------------------------------------------
 * Helper: convert a JSON C-string to a Datum suitable for
 * PG_RETURN_JSONB_P.
 * ---------------------------------------------------------------- */
static Datum
cstring_to_jsonb_datum(const char *json_str)
{
    return DirectFunctionCall1(jsonb_in,
                               CStringGetDatum(json_str));
}

/* ----------------------------------------------------------------
 * Helper: append a JSON key:value pair (string value) to buf.
 * If val is NULL, emits null literal.  Handles the leading comma.
 * ---------------------------------------------------------------- */
static void
append_json_kv_str(StringInfo buf, const char *key, const char *val,
                   bool *first)
{
    if (!*first)
        appendStringInfoChar(buf, ',');
    *first = false;

    appendStringInfo(buf, "\"%s\":", key);
    if (val)
    {
        char *esc = sage_escape_json_string(val);
        appendStringInfo(buf, "\"%s\"", esc);
        pfree(esc);
    }
    else
        appendStringInfoString(buf, "null");
}

/* Same but for raw JSON (number, bool, object, array — no quotes). */
static void
append_json_kv_raw(StringInfo buf, const char *key, const char *val,
                   bool *first)
{
    if (!*first)
        appendStringInfoChar(buf, ',');
    *first = false;

    appendStringInfo(buf, "\"%s\":", key);
    if (val && val[0] != '\0')
        appendStringInfoString(buf, val);
    else
        appendStringInfoString(buf, "null");
}

/* ================================================================
 * 1. sage_health_json()
 *
 * Returns a JSONB object with system health overview combining
 * sage.status() shared-memory state with live database health.
 * ================================================================ */
PG_FUNCTION_INFO_V1(sage_health_json);

Datum
sage_health_json(PG_FUNCTION_ARGS)
{
    StringInfoData  buf;
    int             ret;
    const char     *circuit_str;
    const char     *llm_circuit_str;
    bool            first = true;

    /* Local copies from shared state */
    bool        local_enabled = false;
    bool        local_collector_running = false;
    bool        local_analyzer_running = false;
    bool        local_briefing_running = false;
    bool        local_emergency_stopped = false;
    SageCircuitState local_circuit = SAGE_CIRCUIT_CLOSED;
    SageCircuitState local_llm_circuit = SAGE_CIRCUIT_CLOSED;

    initStringInfo(&buf);

    /* Read shared state if available */
    if (sage_state != NULL)
    {
        LWLockAcquire(sage_state->lock, LW_SHARED);
        local_circuit           = sage_state->circuit_state;
        local_llm_circuit       = sage_state->llm_circuit_state;
        local_collector_running = sage_state->collector_running;
        local_analyzer_running  = sage_state->analyzer_running;
        local_briefing_running  = sage_state->briefing_running;
        local_emergency_stopped = sage_state->emergency_stopped;
        LWLockRelease(sage_state->lock);
    }
    local_enabled = sage_enabled;

    switch (local_circuit)
    {
        case SAGE_CIRCUIT_CLOSED:  circuit_str = "closed";  break;
        case SAGE_CIRCUIT_OPEN:    circuit_str = "open";    break;
        case SAGE_CIRCUIT_DORMANT: circuit_str = "dormant"; break;
        default:                   circuit_str = "unknown"; break;
    }
    switch (local_llm_circuit)
    {
        case SAGE_CIRCUIT_CLOSED:  llm_circuit_str = "closed";  break;
        case SAGE_CIRCUIT_OPEN:    llm_circuit_str = "open";    break;
        case SAGE_CIRCUIT_DORMANT: llm_circuit_str = "dormant"; break;
        default:                   llm_circuit_str = "unknown"; break;
    }

    appendStringInfoChar(&buf, '{');

    /* Static fields from shared memory */
    append_json_kv_str(&buf, "version", PG_SAGE_VERSION, &first);
    append_json_kv_raw(&buf, "enabled", local_enabled ? "true" : "false", &first);
    append_json_kv_str(&buf, "circuit_state", circuit_str, &first);
    append_json_kv_str(&buf, "llm_circuit_state", llm_circuit_str, &first);
    append_json_kv_raw(&buf, "emergency_stopped",
                       local_emergency_stopped ? "true" : "false", &first);

    /* Workers */
    appendStringInfoString(&buf, ",\"workers\":{");
    appendStringInfo(&buf, "\"collector\":%s",
                     local_collector_running ? "true" : "false");
    appendStringInfo(&buf, ",\"analyzer\":%s",
                     local_analyzer_running ? "true" : "false");
    appendStringInfo(&buf, ",\"briefing\":%s",
                     local_briefing_running ? "true" : "false");
    appendStringInfoChar(&buf, '}');

    /* Database health via SPI */
    if (SPI_connect() == SPI_OK_CONNECT)
    {
        /* Connections */
        ret = SPI_execute(
            "SELECT count(*) AS total, "
            "  count(*) FILTER (WHERE state = 'active') AS active, "
            "  current_setting('max_connections')::int AS max_conn "
            "FROM pg_stat_activity", true, 0);

        if (ret == SPI_OK_SELECT && SPI_processed > 0)
        {
            char *total    = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1);
            char *active   = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 2);
            char *max_conn = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 3);

            appendStringInfoString(&buf, ",\"connections\":{");
            appendStringInfo(&buf, "\"total\":%s", total ? total : "0");
            appendStringInfo(&buf, ",\"active\":%s", active ? active : "0");
            appendStringInfo(&buf, ",\"max\":%s", max_conn ? max_conn : "0");
            appendStringInfoChar(&buf, '}');
        }

        /* Cache hit ratio */
        ret = SPI_execute(
            "SELECT round(100.0 * sum(blks_hit) / "
            "  NULLIF(sum(blks_hit) + sum(blks_read), 0), 2)::text AS ratio "
            "FROM pg_stat_database", true, 0);

        if (ret == SPI_OK_SELECT && SPI_processed > 0)
        {
            char *ratio = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1);
            append_json_kv_raw(&buf, "cache_hit_ratio_pct", ratio, &first);
        }

        /* Replication lag (returns null on standalone) */
        ret = SPI_execute(
            "SELECT CASE WHEN pg_is_in_recovery() "
            "  THEN extract(epoch FROM now() - pg_last_xact_replay_timestamp())::text "
            "  ELSE NULL END AS lag_seconds", true, 0);

        if (ret == SPI_OK_SELECT && SPI_processed > 0)
        {
            char *lag = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1);
            append_json_kv_raw(&buf, "replication_lag_seconds", lag, &first);
        }

        /* Database size */
        ret = SPI_execute(
            "SELECT pg_size_pretty(pg_database_size(current_database())) AS size, "
            "  pg_database_size(current_database())::text AS size_bytes "
            "FROM pg_database WHERE datname = current_database()", true, 0);

        if (ret == SPI_OK_SELECT && SPI_processed > 0)
        {
            char *pretty = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1);
            char *bytes  = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 2);

            appendStringInfoString(&buf, ",\"disk\":{");
            appendStringInfo(&buf, "\"database_size\":\"%s\"",
                             pretty ? pretty : "unknown");
            appendStringInfo(&buf, ",\"database_size_bytes\":%s",
                             bytes ? bytes : "0");
            appendStringInfoChar(&buf, '}');
        }

        SPI_finish();
    }

    appendStringInfoChar(&buf, '}');

    PG_RETURN_DATUM(cstring_to_jsonb_datum(buf.data));
}

/* ================================================================
 * 2. sage_findings_json(status_filter TEXT DEFAULT 'open')
 *
 * Returns a JSONB array of findings filtered by status.
 * ================================================================ */
PG_FUNCTION_INFO_V1(sage_findings_json);

Datum
sage_findings_json(PG_FUNCTION_ARGS)
{
    StringInfoData  buf;
    char           *status_filter;
    int             ret;
    int             i;

    if (PG_ARGISNULL(0))
        status_filter = "open";
    else
        status_filter = text_to_cstring(PG_GETARG_TEXT_PP(0));

    initStringInfo(&buf);
    appendStringInfoChar(&buf, '[');

    if (SPI_connect() != SPI_OK_CONNECT)
    {
        appendStringInfoChar(&buf, ']');
        PG_RETURN_DATUM(cstring_to_jsonb_datum(buf.data));
    }

    {
        Oid     argtypes[1] = {TEXTOID};
        Datum   values[1];
        char    nulls[1] = {' '};

        values[0] = CStringGetTextDatum(status_filter);

        ret = SPI_execute_with_args(
            "SELECT id, created_at::text, last_seen::text, occurrence_count, "
            "  category, severity, object_type, object_identifier, "
            "  title, detail::text, recommendation, recommended_sql, "
            "  rollback_sql, status, "
            "  suppressed_until::text, resolved_at::text, acted_on_at::text "
            "FROM sage.findings "
            "WHERE status = $1 "
            "ORDER BY CASE severity "
            "  WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END, "
            "  last_seen DESC",
            1, argtypes, values, nulls, true, 0);
    }

    if (ret == SPI_OK_SELECT && SPI_tuptable != NULL)
    {
        for (i = 0; i < (int) SPI_processed; i++)
        {
            char *id_str          = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 1);
            char *created_at      = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 2);
            char *last_seen       = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 3);
            char *occ_count       = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 4);
            char *category        = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 5);
            char *severity        = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 6);
            char *obj_type        = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 7);
            char *obj_id          = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 8);
            char *title           = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 9);
            char *detail          = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 10);
            char *recommendation  = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 11);
            char *rec_sql         = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 12);
            char *roll_sql        = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 13);
            char *status_val      = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 14);
            char *suppressed      = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 15);
            char *resolved_at     = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 16);
            char *acted_on_at     = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 17);

            bool f = true;

            if (i > 0)
                appendStringInfoChar(&buf, ',');

            appendStringInfoChar(&buf, '{');
            append_json_kv_raw(&buf, "id", id_str, &f);
            append_json_kv_str(&buf, "created_at", created_at, &f);
            append_json_kv_str(&buf, "last_seen", last_seen, &f);
            append_json_kv_raw(&buf, "occurrence_count", occ_count, &f);
            append_json_kv_str(&buf, "category", category, &f);
            append_json_kv_str(&buf, "severity", severity, &f);
            append_json_kv_str(&buf, "object_type", obj_type, &f);
            append_json_kv_str(&buf, "object_identifier", obj_id, &f);
            append_json_kv_str(&buf, "title", title, &f);

            /* detail is already a JSON string from the DB, emit raw */
            append_json_kv_raw(&buf, "detail", detail, &f);

            append_json_kv_str(&buf, "recommendation", recommendation, &f);
            append_json_kv_str(&buf, "recommended_sql", rec_sql, &f);
            append_json_kv_str(&buf, "rollback_sql", roll_sql, &f);
            append_json_kv_str(&buf, "status", status_val, &f);
            append_json_kv_str(&buf, "suppressed_until", suppressed, &f);
            append_json_kv_str(&buf, "resolved_at", resolved_at, &f);
            append_json_kv_str(&buf, "acted_on_at", acted_on_at, &f);
            appendStringInfoChar(&buf, '}');
        }
    }

    SPI_finish();
    appendStringInfoChar(&buf, ']');

    PG_RETURN_DATUM(cstring_to_jsonb_datum(buf.data));
}

/* ================================================================
 * 3. sage_schema_json(table_name TEXT)
 *
 * Returns DDL, indexes, constraints, columns, foreign keys
 * for a specific table.
 * ================================================================ */
PG_FUNCTION_INFO_V1(sage_schema_json);

Datum
sage_schema_json(PG_FUNCTION_ARGS)
{
    StringInfoData  buf;
    char           *table_name;
    int             ret;
    int             i;
    Oid             argtypes[1] = {TEXTOID};
    Datum           values[1];
    char            nulls[1] = {' '};

    if (PG_ARGISNULL(0))
        ereport(ERROR,
                (errcode(ERRCODE_NULL_VALUE_NOT_ALLOWED),
                 errmsg("table_name must not be NULL")));

    table_name = text_to_cstring(PG_GETARG_TEXT_PP(0));
    values[0] = CStringGetTextDatum(table_name);

    initStringInfo(&buf);
    appendStringInfoChar(&buf, '{');

    {
        bool first = true;
        append_json_kv_str(&buf, "table_name", table_name, &first);
    }

    if (SPI_connect() != SPI_OK_CONNECT)
    {
        appendStringInfoString(&buf, "}");
        PG_RETURN_DATUM(cstring_to_jsonb_datum(buf.data));
    }

    /* Columns */
    ret = SPI_execute_with_args(
        "SELECT c.column_name, c.data_type, c.is_nullable, "
        "  c.column_default, c.character_maximum_length::text "
        "FROM information_schema.columns c "
        "WHERE c.table_schema || '.' || c.table_name = $1 "
        "   OR c.table_name = $1 "
        "ORDER BY c.ordinal_position",
        1, argtypes, values, nulls, true, 0);

    appendStringInfoString(&buf, ",\"columns\":[");
    if (ret == SPI_OK_SELECT && SPI_tuptable != NULL)
    {
        for (i = 0; i < (int) SPI_processed; i++)
        {
            char *col_name  = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 1);
            char *data_type = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 2);
            char *nullable  = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 3);
            char *defval    = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 4);
            char *max_len   = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 5);
            bool f = true;

            if (i > 0)
                appendStringInfoChar(&buf, ',');

            appendStringInfoChar(&buf, '{');
            append_json_kv_str(&buf, "name", col_name, &f);
            append_json_kv_str(&buf, "type", data_type, &f);
            append_json_kv_str(&buf, "nullable", nullable, &f);
            append_json_kv_str(&buf, "default", defval, &f);
            append_json_kv_raw(&buf, "max_length", max_len, &f);
            appendStringInfoChar(&buf, '}');
        }
    }
    appendStringInfoChar(&buf, ']');

    /* Indexes */
    ret = SPI_execute_with_args(
        "SELECT indexname, indexdef "
        "FROM pg_indexes "
        "WHERE schemaname || '.' || tablename = $1 "
        "   OR tablename = $1 "
        "ORDER BY indexname",
        1, argtypes, values, nulls, true, 0);

    appendStringInfoString(&buf, ",\"indexes\":[");
    if (ret == SPI_OK_SELECT && SPI_tuptable != NULL)
    {
        for (i = 0; i < (int) SPI_processed; i++)
        {
            char *iname = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 1);
            char *idef  = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 2);
            bool f = true;

            if (i > 0)
                appendStringInfoChar(&buf, ',');

            appendStringInfoChar(&buf, '{');
            append_json_kv_str(&buf, "name", iname, &f);
            append_json_kv_str(&buf, "definition", idef, &f);
            appendStringInfoChar(&buf, '}');
        }
    }
    appendStringInfoChar(&buf, ']');

    /* Constraints (PK, UNIQUE, CHECK, EXCLUDE) */
    ret = SPI_execute_with_args(
        "SELECT con.conname, con.contype::text, "
        "  pg_get_constraintdef(con.oid) AS definition "
        "FROM pg_constraint con "
        "JOIN pg_class rel ON rel.oid = con.conrelid "
        "JOIN pg_namespace nsp ON nsp.oid = rel.relnamespace "
        "WHERE nsp.nspname || '.' || rel.relname = $1 "
        "   OR rel.relname = $1 "
        "ORDER BY con.conname",
        1, argtypes, values, nulls, true, 0);

    appendStringInfoString(&buf, ",\"constraints\":[");
    if (ret == SPI_OK_SELECT && SPI_tuptable != NULL)
    {
        for (i = 0; i < (int) SPI_processed; i++)
        {
            char *cname = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 1);
            char *ctype = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 2);
            char *cdef  = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 3);
            bool f = true;

            if (i > 0)
                appendStringInfoChar(&buf, ',');

            appendStringInfoChar(&buf, '{');
            append_json_kv_str(&buf, "name", cname, &f);
            append_json_kv_str(&buf, "type", ctype, &f);
            append_json_kv_str(&buf, "definition", cdef, &f);
            appendStringInfoChar(&buf, '}');
        }
    }
    appendStringInfoChar(&buf, ']');

    /* Foreign keys */
    ret = SPI_execute_with_args(
        "SELECT con.conname, "
        "  pg_get_constraintdef(con.oid) AS definition, "
        "  ref_nsp.nspname || '.' || ref_rel.relname AS references_table "
        "FROM pg_constraint con "
        "JOIN pg_class rel ON rel.oid = con.conrelid "
        "JOIN pg_namespace nsp ON nsp.oid = rel.relnamespace "
        "LEFT JOIN pg_class ref_rel ON ref_rel.oid = con.confrelid "
        "LEFT JOIN pg_namespace ref_nsp ON ref_nsp.oid = ref_rel.relnamespace "
        "WHERE con.contype = 'f' "
        "  AND (nsp.nspname || '.' || rel.relname = $1 OR rel.relname = $1) "
        "ORDER BY con.conname",
        1, argtypes, values, nulls, true, 0);

    appendStringInfoString(&buf, ",\"foreign_keys\":[");
    if (ret == SPI_OK_SELECT && SPI_tuptable != NULL)
    {
        for (i = 0; i < (int) SPI_processed; i++)
        {
            char *fkname  = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 1);
            char *fkdef   = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 2);
            char *ref_tbl = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 3);
            bool f = true;

            if (i > 0)
                appendStringInfoChar(&buf, ',');

            appendStringInfoChar(&buf, '{');
            append_json_kv_str(&buf, "name", fkname, &f);
            append_json_kv_str(&buf, "definition", fkdef, &f);
            append_json_kv_str(&buf, "references_table", ref_tbl, &f);
            appendStringInfoChar(&buf, '}');
        }
    }
    appendStringInfoChar(&buf, ']');

    SPI_finish();
    appendStringInfoChar(&buf, '}');

    PG_RETURN_DATUM(cstring_to_jsonb_datum(buf.data));
}

/* ================================================================
 * 4. sage_stats_json(table_name TEXT)
 *
 * Table size, row counts, dead tuples, index usage, vacuum status.
 * ================================================================ */
PG_FUNCTION_INFO_V1(sage_stats_json);

Datum
sage_stats_json(PG_FUNCTION_ARGS)
{
    StringInfoData  buf;
    char           *table_name;
    int             ret;
    Oid             argtypes[1] = {TEXTOID};
    Datum           values[1];
    char            nulls[1] = {' '};
    bool            first = true;

    if (PG_ARGISNULL(0))
        ereport(ERROR,
                (errcode(ERRCODE_NULL_VALUE_NOT_ALLOWED),
                 errmsg("table_name must not be NULL")));

    table_name = text_to_cstring(PG_GETARG_TEXT_PP(0));
    values[0] = CStringGetTextDatum(table_name);

    initStringInfo(&buf);
    appendStringInfoChar(&buf, '{');
    append_json_kv_str(&buf, "table_name", table_name, &first);

    if (SPI_connect() != SPI_OK_CONNECT)
    {
        appendStringInfoChar(&buf, '}');
        PG_RETURN_DATUM(cstring_to_jsonb_datum(buf.data));
    }

    /* Table size */
    ret = SPI_execute_with_args(
        "SELECT pg_size_pretty(pg_total_relation_size(c.oid)) AS total_size, "
        "  pg_total_relation_size(c.oid)::text AS total_bytes, "
        "  pg_size_pretty(pg_relation_size(c.oid)) AS table_size, "
        "  pg_size_pretty(pg_indexes_size(c.oid)) AS indexes_size "
        "FROM pg_class c "
        "JOIN pg_namespace n ON n.oid = c.relnamespace "
        "WHERE n.nspname || '.' || c.relname = $1 "
        "   OR c.relname = $1 "
        "LIMIT 1",
        1, argtypes, values, nulls, true, 0);

    if (ret == SPI_OK_SELECT && SPI_processed > 0)
    {
        char *total_pretty = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1);
        char *total_bytes  = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 2);
        char *table_sz     = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 3);
        char *idx_sz       = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 4);

        append_json_kv_str(&buf, "total_size", total_pretty, &first);
        append_json_kv_raw(&buf, "total_bytes", total_bytes, &first);
        append_json_kv_str(&buf, "table_size", table_sz, &first);
        append_json_kv_str(&buf, "indexes_size", idx_sz, &first);
    }

    /* Stats from pg_stat_user_tables */
    ret = SPI_execute_with_args(
        "SELECT s.n_live_tup::text, s.n_dead_tup::text, "
        "  s.seq_scan::text, s.seq_tup_read::text, "
        "  s.idx_scan::text, s.idx_tup_fetch::text, "
        "  s.n_tup_ins::text, s.n_tup_upd::text, s.n_tup_del::text, "
        "  s.last_vacuum::text, s.last_autovacuum::text, "
        "  s.last_analyze::text, s.last_autoanalyze::text, "
        "  s.vacuum_count::text, s.autovacuum_count::text "
        "FROM pg_stat_user_tables s "
        "WHERE s.schemaname || '.' || s.relname = $1 "
        "   OR s.relname = $1 "
        "LIMIT 1",
        1, argtypes, values, nulls, true, 0);

    if (ret == SPI_OK_SELECT && SPI_processed > 0)
    {
        char *live_tup      = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1);
        char *dead_tup      = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 2);
        char *seq_scan      = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 3);
        char *seq_read      = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 4);
        char *idx_scan      = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 5);
        char *idx_fetch     = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 6);
        char *ins           = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 7);
        char *upd           = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 8);
        char *del           = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 9);
        char *last_vac      = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 10);
        char *last_autovac  = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 11);
        char *last_anl      = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 12);
        char *last_autoanl  = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 13);
        char *vac_count     = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 14);
        char *autovac_count = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 15);

        append_json_kv_raw(&buf, "live_tuples", live_tup, &first);
        append_json_kv_raw(&buf, "dead_tuples", dead_tup, &first);
        append_json_kv_raw(&buf, "seq_scan", seq_scan, &first);
        append_json_kv_raw(&buf, "seq_tup_read", seq_read, &first);
        append_json_kv_raw(&buf, "idx_scan", idx_scan, &first);
        append_json_kv_raw(&buf, "idx_tup_fetch", idx_fetch, &first);
        append_json_kv_raw(&buf, "n_tup_ins", ins, &first);
        append_json_kv_raw(&buf, "n_tup_upd", upd, &first);
        append_json_kv_raw(&buf, "n_tup_del", del, &first);
        append_json_kv_str(&buf, "last_vacuum", last_vac, &first);
        append_json_kv_str(&buf, "last_autovacuum", last_autovac, &first);
        append_json_kv_str(&buf, "last_analyze", last_anl, &first);
        append_json_kv_str(&buf, "last_autoanalyze", last_autoanl, &first);
        append_json_kv_raw(&buf, "vacuum_count", vac_count, &first);
        append_json_kv_raw(&buf, "autovacuum_count", autovac_count, &first);
    }

    /* Dead tuple ratio */
    ret = SPI_execute_with_args(
        "SELECT CASE WHEN n_live_tup + n_dead_tup > 0 "
        "  THEN round(100.0 * n_dead_tup / (n_live_tup + n_dead_tup), 2)::text "
        "  ELSE '0' END AS dead_ratio "
        "FROM pg_stat_user_tables "
        "WHERE schemaname || '.' || relname = $1 "
        "   OR relname = $1 "
        "LIMIT 1",
        1, argtypes, values, nulls, true, 0);

    if (ret == SPI_OK_SELECT && SPI_processed > 0)
    {
        char *ratio = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1);
        append_json_kv_raw(&buf, "dead_tuple_ratio_pct", ratio, &first);
    }

    SPI_finish();
    appendStringInfoChar(&buf, '}');

    PG_RETURN_DATUM(cstring_to_jsonb_datum(buf.data));
}

/* ================================================================
 * 5. sage_slow_queries_json()
 *
 * Top slow queries from pg_stat_statements (if available).
 * ================================================================ */
PG_FUNCTION_INFO_V1(sage_slow_queries_json);

Datum
sage_slow_queries_json(PG_FUNCTION_ARGS)
{
    StringInfoData  buf;
    int             ret;
    int             i;

    initStringInfo(&buf);
    appendStringInfoChar(&buf, '[');

    if (SPI_connect() != SPI_OK_CONNECT)
    {
        appendStringInfoChar(&buf, ']');
        PG_RETURN_DATUM(cstring_to_jsonb_datum(buf.data));
    }

    /* Try pg_stat_statements; it may not be installed */
    PG_TRY();
    {
        ret = SPI_execute(
            "SELECT queryid::text, "
            "  left(query, 500) AS query, "
            "  calls::text, "
            "  round((total_exec_time)::numeric, 2)::text AS total_ms, "
            "  round((mean_exec_time)::numeric, 2)::text AS mean_ms, "
            "  round((max_exec_time)::numeric, 2)::text AS max_ms, "
            "  round((min_exec_time)::numeric, 2)::text AS min_ms, "
            "  round((stddev_exec_time)::numeric, 2)::text AS stddev_ms, "
            "  rows::text "
            "FROM pg_stat_statements "
            "WHERE userid = (SELECT oid FROM pg_roles WHERE rolname = current_user) "
            "   OR true "
            "ORDER BY total_exec_time DESC "
            "LIMIT 20",
            true, 0);

        if (ret == SPI_OK_SELECT && SPI_tuptable != NULL)
        {
            for (i = 0; i < (int) SPI_processed; i++)
            {
                char *qid       = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 1);
                char *query     = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 2);
                char *calls     = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 3);
                char *total_ms  = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 4);
                char *mean_ms   = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 5);
                char *max_ms    = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 6);
                char *min_ms    = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 7);
                char *stddev_ms = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 8);
                char *rows_str  = SPI_getvalue(SPI_tuptable->vals[i], SPI_tuptable->tupdesc, 9);
                bool f = true;

                if (i > 0)
                    appendStringInfoChar(&buf, ',');

                appendStringInfoChar(&buf, '{');
                append_json_kv_raw(&buf, "queryid", qid, &f);
                append_json_kv_str(&buf, "query", query, &f);
                append_json_kv_raw(&buf, "calls", calls, &f);
                append_json_kv_raw(&buf, "total_exec_time_ms", total_ms, &f);
                append_json_kv_raw(&buf, "mean_exec_time_ms", mean_ms, &f);
                append_json_kv_raw(&buf, "max_exec_time_ms", max_ms, &f);
                append_json_kv_raw(&buf, "min_exec_time_ms", min_ms, &f);
                append_json_kv_raw(&buf, "stddev_exec_time_ms", stddev_ms, &f);
                append_json_kv_raw(&buf, "rows", rows_str, &f);
                appendStringInfoChar(&buf, '}');
            }
        }
    }
    PG_CATCH();
    {
        /* pg_stat_statements not available — return empty array */
        FlushErrorState();
    }
    PG_END_TRY();

    SPI_finish();
    appendStringInfoChar(&buf, ']');

    PG_RETURN_DATUM(cstring_to_jsonb_datum(buf.data));
}

/* ================================================================
 * 6. sage_explain_json(qid BIGINT)
 *
 * Returns cached explain plan from sage.explain_cache, or null.
 * ================================================================ */
PG_FUNCTION_INFO_V1(sage_explain_json);

Datum
sage_explain_json(PG_FUNCTION_ARGS)
{
    StringInfoData  buf;
    int64           queryid;
    int             ret;
    bool            first = true;

    if (PG_ARGISNULL(0))
        PG_RETURN_NULL();

    queryid = PG_GETARG_INT64(0);

    initStringInfo(&buf);

    if (SPI_connect() != SPI_OK_CONNECT)
        PG_RETURN_NULL();

    {
        Oid     argtypes[1] = {INT8OID};
        Datum   values[1];
        char    nulls[1] = {' '};

        values[0] = Int64GetDatum(queryid);

        ret = SPI_execute_with_args(
            "SELECT captured_at::text, queryid::text, query_text, "
            "  plan_json::text, source, "
            "  total_cost::text, execution_time::text "
            "FROM sage.explain_cache "
            "WHERE queryid = $1 "
            "ORDER BY captured_at DESC "
            "LIMIT 1",
            1, argtypes, values, nulls, true, 0);
    }

    if (ret != SPI_OK_SELECT || SPI_processed == 0)
    {
        SPI_finish();
        PG_RETURN_NULL();
    }

    {
        char *captured_at = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1);
        char *qid_str     = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 2);
        char *query_text  = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 3);
        char *plan_json   = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 4);
        char *source      = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 5);
        char *total_cost  = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 6);
        char *exec_time   = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 7);

        appendStringInfoChar(&buf, '{');
        append_json_kv_str(&buf, "captured_at", captured_at, &first);
        append_json_kv_raw(&buf, "queryid", qid_str, &first);
        append_json_kv_str(&buf, "query_text", query_text, &first);

        /* plan_json is already JSON — emit raw */
        append_json_kv_raw(&buf, "plan", plan_json, &first);

        append_json_kv_str(&buf, "source", source, &first);
        append_json_kv_raw(&buf, "total_cost", total_cost, &first);
        append_json_kv_raw(&buf, "execution_time", exec_time, &first);
        appendStringInfoChar(&buf, '}');
    }

    SPI_finish();

    PG_RETURN_DATUM(cstring_to_jsonb_datum(buf.data));
}
