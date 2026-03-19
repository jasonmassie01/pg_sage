/*
 * collector.c — pg_sage Collector Background Worker
 *
 * Core data collection loop that snapshots PostgreSQL statistics
 * into the sage.snapshots table.
 *
 * AGPL-3.0 License
 */

#include "pg_sage.h"

#include "lib/stringinfo.h"
#include "utils/timestamp.h"
#include "tcop/utility.h"
#include "access/xact.h"
#include "utils/snapmgr.h"

#include <signal.h>

/* ----------------------------------------------------------------
 * Signal handling
 * ---------------------------------------------------------------- */
static volatile sig_atomic_t got_sigterm = false;
static volatile sig_atomic_t got_sighup = false;

/* Batch offset for table stats pagination */
static int table_stats_offset = 0;

/* sage_database GUC (defined in guc.c) */
extern char *sage_database;

static void
sage_collector_sigterm(SIGNAL_ARGS)
{
    int save_errno = errno;
    got_sigterm = true;
    SetLatch(MyLatch);
    errno = save_errno;
}

static void
sage_collector_sighup(SIGNAL_ARGS)
{
    int save_errno = errno;
    got_sighup = true;
    SetLatch(MyLatch);
    errno = save_errno;
}

/* ----------------------------------------------------------------
 * Helper: escape a string value for inclusion in JSON
 * ---------------------------------------------------------------- */
static void
json_escape_string(StringInfo buf, const char *str)
{
    if (str == NULL)
    {
        appendStringInfoString(buf, "null");
        return;
    }

    appendStringInfoChar(buf, '"');
    for (const char *p = str; *p; p++)
    {
        switch (*p)
        {
            case '"':
                appendStringInfoString(buf, "\\\"");
                break;
            case '\\':
                appendStringInfoString(buf, "\\\\");
                break;
            case '\b':
                appendStringInfoString(buf, "\\b");
                break;
            case '\f':
                appendStringInfoString(buf, "\\f");
                break;
            case '\n':
                appendStringInfoString(buf, "\\n");
                break;
            case '\r':
                appendStringInfoString(buf, "\\r");
                break;
            case '\t':
                appendStringInfoString(buf, "\\t");
                break;
            default:
                if ((unsigned char)*p < 0x20)
                    appendStringInfo(buf, "\\u%04x", (unsigned char)*p);
                else
                    appendStringInfoChar(buf, *p);
                break;
        }
    }
    appendStringInfoChar(buf, '"');
}

/* ----------------------------------------------------------------
 * Helper: append a JSON key-value pair (string value)
 * ---------------------------------------------------------------- */
static void
json_append_str(StringInfo buf, const char *key, int row, int col, bool *first)
{
    if (!*first)
        appendStringInfoChar(buf, ',');
    *first = false;

    appendStringInfoChar(buf, '"');
    appendStringInfoString(buf, key);
    appendStringInfoString(buf, "\":");

    if (sage_spi_isnull(row, col))
        appendStringInfoString(buf, "null");
    else
        json_escape_string(buf, sage_spi_getval_str(row, col));
}

/* ----------------------------------------------------------------
 * Helper: append a JSON key-value pair (numeric — no quotes)
 * ---------------------------------------------------------------- */
static void
json_append_num(StringInfo buf, const char *key, int row, int col, bool *first)
{
    if (!*first)
        appendStringInfoChar(buf, ',');
    *first = false;

    appendStringInfoChar(buf, '"');
    appendStringInfoString(buf, key);
    appendStringInfoString(buf, "\":");

    if (sage_spi_isnull(row, col))
        appendStringInfoString(buf, "null");
    else
    {
        char *val = sage_spi_getval_str(row, col);
        if (val != NULL)
            appendStringInfoString(buf, val);
        else
            appendStringInfoString(buf, "null");
    }
}

/* ----------------------------------------------------------------
 * Helper: insert a snapshot row
 * ---------------------------------------------------------------- */
static void
insert_snapshot(const char *category, const char *data_json)
{
    static const char *insert_sql =
        "INSERT INTO sage.snapshots (category, data) VALUES ($1, $2::jsonb)";

    Oid     argtypes[2] = {TEXTOID, TEXTOID};
    Datum   values[2];
    char    nulls[2] = {' ', ' '};
    int     ret;

    values[0] = CStringGetTextDatum(category);
    values[1] = CStringGetTextDatum(data_json);

    ret = SPI_execute_with_args(insert_sql, 2, argtypes, values, nulls,
                                false, 0);
    if (ret != SPI_OK_INSERT)
        elog(WARNING, "pg_sage: insert_snapshot failed for category=%s (SPI returned %d)",
             category, ret);
}

/* ----------------------------------------------------------------
 * sage_collect_stat_statements
 * ---------------------------------------------------------------- */
void
sage_collect_stat_statements(void)
{
    volatile bool err = false;
    int         ret;
    StringInfoData buf;

    SetCurrentStatementStartTimestamp();
    StartTransactionCommand();
    PushActiveSnapshot(GetTransactionSnapshot());
    SPI_connect();

    PG_TRY();
    {
        sage_spi_exec("SET LOCAL statement_timeout = '500ms'", 0);

        ret = sage_spi_exec(
            "SELECT queryid, query, calls, total_exec_time, mean_exec_time, "
            "       rows, shared_blks_hit, shared_blks_read, temp_blks_written, "
            "       blk_read_time, blk_write_time "
            "FROM pg_stat_statements s "
            "JOIN pg_database d ON d.oid = s.dbid "
            "WHERE d.datname = current_database() "
            "ORDER BY total_exec_time DESC "
            "LIMIT 500",
            0);

        if (ret >= 0 && SPI_processed > 0)
        {
            uint64 nrows = SPI_processed;

            initStringInfo(&buf);
            appendStringInfoChar(&buf, '[');

            for (uint64 i = 0; i < nrows; i++)
            {
                bool first = true;

                if (i > 0)
                    appendStringInfoChar(&buf, ',');

                appendStringInfoChar(&buf, '{');
                json_append_num(&buf, "queryid",            i, 0, &first);
                json_append_str(&buf, "query",              i, 1, &first);
                json_append_num(&buf, "calls",              i, 2, &first);
                json_append_num(&buf, "total_exec_time",    i, 3, &first);
                json_append_num(&buf, "mean_exec_time",     i, 4, &first);
                json_append_num(&buf, "rows",               i, 5, &first);
                json_append_num(&buf, "shared_blks_hit",    i, 6, &first);
                json_append_num(&buf, "shared_blks_read",   i, 7, &first);
                json_append_num(&buf, "temp_blks_written",  i, 8, &first);
                json_append_num(&buf, "blk_read_time",      i, 9, &first);
                json_append_num(&buf, "blk_write_time",     i, 10, &first);
                appendStringInfoChar(&buf, '}');
            }

            appendStringInfoChar(&buf, ']');

            insert_snapshot("queries", buf.data);
            pfree(buf.data);
        }
    }
    PG_CATCH();
    {
        EmitErrorReport();
        FlushErrorState();
        err = true;
        elog(WARNING, "pg_sage: failed to collect stat_statements");
    }
    PG_END_TRY();

    SPI_finish();
    if (err)
        AbortCurrentTransaction();
    else
    {
        PopActiveSnapshot();
        CommitTransactionCommand();
    }
}

/* ----------------------------------------------------------------
 * sage_collect_table_stats
 * ---------------------------------------------------------------- */
void
sage_collect_table_stats(void)
{
    volatile bool err = false;
    int         ret;
    StringInfoData query;
    StringInfoData buf;

    SetCurrentStatementStartTimestamp();
    StartTransactionCommand();
    PushActiveSnapshot(GetTransactionSnapshot());
    SPI_connect();

    PG_TRY();
    {
        sage_spi_exec("SET LOCAL statement_timeout = '500ms'", 0);

        initStringInfo(&query);
        appendStringInfo(&query,
            "SELECT schemaname, relname, seq_scan, seq_tup_read, idx_scan, idx_tup_fetch, "
            "       n_tup_ins, n_tup_upd, n_tup_del, n_live_tup, n_dead_tup, "
            "       last_vacuum, last_autovacuum, last_analyze, last_autoanalyze, "
            "       vacuum_count, autovacuum_count, pg_total_relation_size(relid) as total_bytes "
            "FROM pg_stat_user_tables "
            "ORDER BY schemaname, relname "
            "LIMIT %d OFFSET %d",
            sage_collector_batch_size, table_stats_offset);

        ret = sage_spi_exec(query.data, 0);
        pfree(query.data);

        if (ret >= 0 && SPI_processed > 0)
        {
            uint64 nrows = SPI_processed;

            initStringInfo(&buf);
            appendStringInfoChar(&buf, '[');

            for (uint64 i = 0; i < nrows; i++)
            {
                bool first = true;

                if (i > 0)
                    appendStringInfoChar(&buf, ',');

                appendStringInfoChar(&buf, '{');
                json_append_str(&buf, "schemaname",         i, 0, &first);
                json_append_str(&buf, "relname",            i, 1, &first);
                json_append_num(&buf, "seq_scan",           i, 2, &first);
                json_append_num(&buf, "seq_tup_read",       i, 3, &first);
                json_append_num(&buf, "idx_scan",           i, 4, &first);
                json_append_num(&buf, "idx_tup_fetch",      i, 5, &first);
                json_append_num(&buf, "n_tup_ins",          i, 6, &first);
                json_append_num(&buf, "n_tup_upd",          i, 7, &first);
                json_append_num(&buf, "n_tup_del",          i, 8, &first);
                json_append_num(&buf, "n_live_tup",         i, 9, &first);
                json_append_num(&buf, "n_dead_tup",         i, 10, &first);
                json_append_str(&buf, "last_vacuum",        i, 11, &first);
                json_append_str(&buf, "last_autovacuum",    i, 12, &first);
                json_append_str(&buf, "last_analyze",       i, 13, &first);
                json_append_str(&buf, "last_autoanalyze",   i, 14, &first);
                json_append_num(&buf, "vacuum_count",       i, 15, &first);
                json_append_num(&buf, "autovacuum_count",   i, 16, &first);
                json_append_num(&buf, "total_bytes",        i, 17, &first);
                appendStringInfoChar(&buf, '}');
            }

            appendStringInfoChar(&buf, ']');

            insert_snapshot("tables", buf.data);
            pfree(buf.data);

            /* Advance offset for next cycle; wrap if we got fewer rows than batch size */
            if ((uint64) SPI_processed < (uint64) sage_collector_batch_size)
                table_stats_offset = 0;
            else
                table_stats_offset += sage_collector_batch_size;
        }
        else
        {
            /* No rows returned — reset offset */
            table_stats_offset = 0;
        }
    }
    PG_CATCH();
    {
        EmitErrorReport();
        FlushErrorState();
        err = true;
        elog(WARNING, "pg_sage: failed to collect table stats");
    }
    PG_END_TRY();

    SPI_finish();
    if (err)
        AbortCurrentTransaction();
    else
    {
        PopActiveSnapshot();
        CommitTransactionCommand();
    }
}

/* ----------------------------------------------------------------
 * sage_collect_index_stats
 * ---------------------------------------------------------------- */
void
sage_collect_index_stats(void)
{
    volatile bool err = false;
    int         ret;
    StringInfoData buf;

    SetCurrentStatementStartTimestamp();
    StartTransactionCommand();
    PushActiveSnapshot(GetTransactionSnapshot());
    SPI_connect();

    PG_TRY();
    {
        sage_spi_exec("SET LOCAL statement_timeout = '500ms'", 0);

        ret = sage_spi_exec(
            "SELECT schemaname, relname, indexrelname, idx_scan, idx_tup_read, idx_tup_fetch, "
            "       pg_relation_size(indexrelid) as index_bytes "
            "FROM pg_stat_user_indexes "
            "ORDER BY schemaname, relname, indexrelname",
            0);

        if (ret >= 0 && SPI_processed > 0)
        {
            uint64 nrows = SPI_processed;

            initStringInfo(&buf);
            appendStringInfoChar(&buf, '[');

            for (uint64 i = 0; i < nrows; i++)
            {
                bool first = true;

                if (i > 0)
                    appendStringInfoChar(&buf, ',');

                appendStringInfoChar(&buf, '{');
                json_append_str(&buf, "schemaname",     i, 0, &first);
                json_append_str(&buf, "relname",        i, 1, &first);
                json_append_str(&buf, "indexrelname",   i, 2, &first);
                json_append_num(&buf, "idx_scan",       i, 3, &first);
                json_append_num(&buf, "idx_tup_read",   i, 4, &first);
                json_append_num(&buf, "idx_tup_fetch",  i, 5, &first);
                json_append_num(&buf, "index_bytes",    i, 6, &first);
                appendStringInfoChar(&buf, '}');
            }

            appendStringInfoChar(&buf, ']');

            insert_snapshot("indexes", buf.data);
            pfree(buf.data);
        }
    }
    PG_CATCH();
    {
        EmitErrorReport();
        FlushErrorState();
        err = true;
        elog(WARNING, "pg_sage: failed to collect index stats");
    }
    PG_END_TRY();

    SPI_finish();
    if (err)
        AbortCurrentTransaction();
    else
    {
        PopActiveSnapshot();
        CommitTransactionCommand();
    }
}

/* ----------------------------------------------------------------
 * sage_collect_system_stats
 * ---------------------------------------------------------------- */
void
sage_collect_system_stats(void)
{
    volatile bool err = false;
    int         ret;
    StringInfoData buf;

    SetCurrentStatementStartTimestamp();
    StartTransactionCommand();
    PushActiveSnapshot(GetTransactionSnapshot());
    SPI_connect();

    PG_TRY();
    {
        bool first = true;

        sage_spi_exec("SET LOCAL statement_timeout = '500ms'", 0);

        initStringInfo(&buf);
        appendStringInfoChar(&buf, '{');

        /* --- checkpoint/bgwriter stats --- */
#if PG_VERSION_NUM >= 170000
        ret = sage_spi_exec("SELECT * FROM pg_stat_checkpointer", 0);
#else
        ret = sage_spi_exec("SELECT * FROM pg_stat_bgwriter", 0);
#endif
        if (ret >= 0 && SPI_processed > 0)
        {
            TupleDesc tupdesc = SPI_tuptable->tupdesc;
            int ncols = tupdesc->natts;

            appendStringInfoString(&buf, "\"bgwriter\":{");
            for (int c = 1; c <= ncols; c++)
            {
                char *colname = SPI_fname(tupdesc, c);
                bool is_first_col = (c == 1);

                if (!is_first_col)
                    appendStringInfoChar(&buf, ',');

                appendStringInfoChar(&buf, '"');
                appendStringInfoString(&buf, colname);
                appendStringInfoString(&buf, "\":");

                if (sage_spi_isnull(0, c - 1))
                    appendStringInfoString(&buf, "null");
                else
                {
                    char *val = sage_spi_getval_str(0, c - 1);
                    if (val == NULL)
                    {
                        appendStringInfoString(&buf, "null");
                    }
                    else
                    {
                        /* Try to determine if value is numeric */
                        Oid typid = SPI_gettypeid(tupdesc, c);
                        if (typid == INT2OID || typid == INT4OID || typid == INT8OID ||
                            typid == FLOAT4OID || typid == FLOAT8OID || typid == NUMERICOID)
                            appendStringInfoString(&buf, val);
                        else
                            json_escape_string(&buf, val);
                    }
                }
            }
            appendStringInfoChar(&buf, '}');
            first = false;
        }

        /* --- pg_stat_database for current db --- */
        ret = sage_spi_exec(
            "SELECT numbackends, xact_commit, xact_rollback, blks_read, blks_hit, "
            "       tup_returned, tup_fetched, tup_inserted, tup_updated, tup_deleted, "
            "       conflicts, temp_files, temp_bytes, deadlocks "
            "FROM pg_stat_database WHERE datname = current_database()",
            0);

        if (ret >= 0 && SPI_processed > 0)
        {
            bool db_first = true;

            if (!first)
                appendStringInfoChar(&buf, ',');
            first = false;

            appendStringInfoString(&buf, "\"database\":{");
            json_append_num(&buf, "numbackends",    0, 0, &db_first);
            json_append_num(&buf, "xact_commit",    0, 1, &db_first);
            json_append_num(&buf, "xact_rollback",  0, 2, &db_first);
            json_append_num(&buf, "blks_read",      0, 3, &db_first);
            json_append_num(&buf, "blks_hit",       0, 4, &db_first);
            json_append_num(&buf, "tup_returned",   0, 5, &db_first);
            json_append_num(&buf, "tup_fetched",    0, 6, &db_first);
            json_append_num(&buf, "tup_inserted",   0, 7, &db_first);
            json_append_num(&buf, "tup_updated",    0, 8, &db_first);
            json_append_num(&buf, "tup_deleted",    0, 9, &db_first);
            json_append_num(&buf, "conflicts",      0, 10, &db_first);
            json_append_num(&buf, "temp_files",     0, 11, &db_first);
            json_append_num(&buf, "temp_bytes",     0, 12, &db_first);
            json_append_num(&buf, "deadlocks",      0, 13, &db_first);
            appendStringInfoChar(&buf, '}');
        }

        /* --- active backends count --- */
        ret = sage_spi_exec(
            "SELECT count(*) as active_backends "
            "FROM pg_stat_activity WHERE state = 'active' AND pid != pg_backend_pid()",
            0);

        if (ret >= 0 && SPI_processed > 0)
        {
            if (!first)
                appendStringInfoChar(&buf, ',');
            first = false;

            appendStringInfoString(&buf, "\"active_backends\":");
            if (sage_spi_isnull(0, 0))
                appendStringInfoString(&buf, "0");
            else
            {
                char *val = sage_spi_getval_str(0, 0);
                appendStringInfoString(&buf, val ? val : "0");
            }
        }

        /* --- max_connections --- */
        ret = sage_spi_exec(
            "SELECT setting::int as max_conn FROM pg_settings WHERE name = 'max_connections'",
            0);

        if (ret >= 0 && SPI_processed > 0)
        {
            if (!first)
                appendStringInfoChar(&buf, ',');
            first = false;

            appendStringInfoString(&buf, "\"max_connections\":");
            if (sage_spi_isnull(0, 0))
                appendStringInfoString(&buf, "null");
            else
            {
                char *val = sage_spi_getval_str(0, 0);
                appendStringInfoString(&buf, val ? val : "null");
            }
        }

        /* --- pg_stat_wal (PG14+) — optional, use savepoint to protect SPI --- */
        sage_spi_exec("SAVEPOINT _sage_wal", 0);
        {
            volatile bool wal_ok = false;

            PG_TRY();
            {
                ret = sage_spi_exec("SELECT * FROM pg_stat_wal", 0);
                if (ret >= 0 && SPI_processed > 0)
                {
                    TupleDesc tupdesc = SPI_tuptable->tupdesc;
                    int ncols = tupdesc->natts;

                    if (!first)
                        appendStringInfoChar(&buf, ',');
                    first = false;

                    appendStringInfoString(&buf, "\"wal\":{");
                    for (int c = 1; c <= ncols; c++)
                    {
                        char *colname = SPI_fname(tupdesc, c);

                        if (c > 1)
                            appendStringInfoChar(&buf, ',');

                        appendStringInfoChar(&buf, '"');
                        appendStringInfoString(&buf, colname);
                        appendStringInfoString(&buf, "\":");

                        if (sage_spi_isnull(0, c - 1))
                            appendStringInfoString(&buf, "null");
                        else
                        {
                            char *val = sage_spi_getval_str(0, c - 1);
                            Oid typid = SPI_gettypeid(tupdesc, c);
                            if (typid == INT2OID || typid == INT4OID || typid == INT8OID ||
                                typid == FLOAT4OID || typid == FLOAT8OID || typid == NUMERICOID)
                                appendStringInfoString(&buf, val ? val : "null");
                            else
                                json_escape_string(&buf, val);
                        }
                    }
                    appendStringInfoChar(&buf, '}');
                    wal_ok = true;
                }
            }
            PG_CATCH();
            {
                /* pg_stat_wal not available on this PG version — rollback savepoint */
                FlushErrorState();
                sage_spi_exec("ROLLBACK TO SAVEPOINT _sage_wal", 0);
            }
            PG_END_TRY();

            if (wal_ok)
                sage_spi_exec("RELEASE SAVEPOINT _sage_wal", 0);
        }

        appendStringInfoChar(&buf, '}');

        insert_snapshot("system", buf.data);
        pfree(buf.data);
    }
    PG_CATCH();
    {
        EmitErrorReport();
        FlushErrorState();
        err = true;
        elog(WARNING, "pg_sage: failed to collect system stats");
    }
    PG_END_TRY();

    SPI_finish();
    if (err)
        AbortCurrentTransaction();
    else
    {
        PopActiveSnapshot();
        CommitTransactionCommand();
    }
}

/* ----------------------------------------------------------------
 * sage_collect_lock_stats
 * ---------------------------------------------------------------- */
void
sage_collect_lock_stats(void)
{
    volatile bool err = false;
    int         ret;
    StringInfoData buf;

    SetCurrentStatementStartTimestamp();
    StartTransactionCommand();
    PushActiveSnapshot(GetTransactionSnapshot());
    SPI_connect();

    PG_TRY();
    {
        sage_spi_exec("SET LOCAL statement_timeout = '500ms'", 0);

        ret = sage_spi_exec(
            "SELECT l.locktype, l.mode, l.granted, l.pid, "
            "       a.query, a.state, a.wait_event_type, a.wait_event, "
            "       a.backend_start, a.query_start, "
            "       c.relname "
            "FROM pg_locks l "
            "LEFT JOIN pg_stat_activity a ON a.pid = l.pid "
            "LEFT JOIN pg_class c ON c.oid = l.relation "
            "WHERE l.pid != pg_backend_pid() "
            "  AND NOT l.granted",
            0);

        if (ret >= 0 && SPI_processed > 0)
        {
            uint64 nrows = SPI_processed;

            initStringInfo(&buf);
            appendStringInfoChar(&buf, '[');

            for (uint64 i = 0; i < nrows; i++)
            {
                bool first = true;

                if (i > 0)
                    appendStringInfoChar(&buf, ',');

                appendStringInfoChar(&buf, '{');
                json_append_str(&buf, "locktype",           i, 0, &first);
                json_append_str(&buf, "mode",               i, 1, &first);
                json_append_str(&buf, "granted",            i, 2, &first);
                json_append_num(&buf, "pid",                i, 3, &first);
                json_append_str(&buf, "query",              i, 4, &first);
                json_append_str(&buf, "state",              i, 5, &first);
                json_append_str(&buf, "wait_event_type",    i, 6, &first);
                json_append_str(&buf, "wait_event",         i, 7, &first);
                json_append_str(&buf, "backend_start",      i, 8, &first);
                json_append_str(&buf, "query_start",        i, 9, &first);
                json_append_str(&buf, "relname",            i, 10, &first);
                appendStringInfoChar(&buf, '}');
            }

            appendStringInfoChar(&buf, ']');

            insert_snapshot("locks", buf.data);
            pfree(buf.data);
        }
    }
    PG_CATCH();
    {
        EmitErrorReport();
        FlushErrorState();
        err = true;
        elog(WARNING, "pg_sage: failed to collect lock stats");
    }
    PG_END_TRY();

    SPI_finish();
    if (err)
        AbortCurrentTransaction();
    else
    {
        PopActiveSnapshot();
        CommitTransactionCommand();
    }
}

/* ----------------------------------------------------------------
 * sage_collect_sequence_stats
 * ---------------------------------------------------------------- */
void
sage_collect_sequence_stats(void)
{
    volatile bool err = false;
    int         ret;
    StringInfoData buf;

    SetCurrentStatementStartTimestamp();
    StartTransactionCommand();
    PushActiveSnapshot(GetTransactionSnapshot());
    SPI_connect();

    PG_TRY();
    {
        sage_spi_exec("SET LOCAL statement_timeout = '500ms'", 0);

        ret = sage_spi_exec(
            "SELECT s.schemaname, s.sequencename, s.last_value, s.start_value, "
            "       s.increment_by, s.max_value, s.min_value, s.data_type, "
            "       CASE WHEN s.max_value > 0 "
            "            THEN (s.last_value::float / s.max_value * 100)::numeric(5,2) "
            "            ELSE 0 END as pct_used "
            "FROM pg_sequences s",
            0);

        if (ret >= 0 && SPI_processed > 0)
        {
            uint64 nrows = SPI_processed;

            initStringInfo(&buf);
            appendStringInfoChar(&buf, '[');

            for (uint64 i = 0; i < nrows; i++)
            {
                bool first = true;

                if (i > 0)
                    appendStringInfoChar(&buf, ',');

                appendStringInfoChar(&buf, '{');
                json_append_str(&buf, "schemaname",     i, 0, &first);
                json_append_str(&buf, "sequencename",   i, 1, &first);
                json_append_num(&buf, "last_value",     i, 2, &first);
                json_append_num(&buf, "start_value",    i, 3, &first);
                json_append_num(&buf, "increment_by",   i, 4, &first);
                json_append_num(&buf, "max_value",      i, 5, &first);
                json_append_num(&buf, "min_value",      i, 6, &first);
                json_append_str(&buf, "data_type",      i, 7, &first);
                json_append_num(&buf, "pct_used",       i, 8, &first);
                appendStringInfoChar(&buf, '}');
            }

            appendStringInfoChar(&buf, ']');

            insert_snapshot("sequences", buf.data);
            pfree(buf.data);
        }
    }
    PG_CATCH();
    {
        EmitErrorReport();
        FlushErrorState();
        err = true;
        elog(WARNING, "pg_sage: failed to collect sequence stats");
    }
    PG_END_TRY();

    SPI_finish();
    if (err)
        AbortCurrentTransaction();
    else
    {
        PopActiveSnapshot();
        CommitTransactionCommand();
    }
}

/* ----------------------------------------------------------------
 * sage_collect_replication_stats
 * ---------------------------------------------------------------- */
void
sage_collect_replication_stats(void)
{
    volatile bool err = false;
    int         ret;
    StringInfoData buf;

    SetCurrentStatementStartTimestamp();
    StartTransactionCommand();
    PushActiveSnapshot(GetTransactionSnapshot());
    SPI_connect();

    PG_TRY();
    {
        bool has_data = false;

        sage_spi_exec("SET LOCAL statement_timeout = '500ms'", 0);

        initStringInfo(&buf);
        appendStringInfoString(&buf, "{\"replication\":");

        /* --- pg_stat_replication --- */
        ret = sage_spi_exec(
            "SELECT pid, usename, application_name, client_addr, state, "
            "       sent_lsn, write_lsn, flush_lsn, replay_lsn, "
            "       write_lag, flush_lag, replay_lag "
            "FROM pg_stat_replication",
            0);

        if (ret >= 0 && SPI_processed > 0)
        {
            uint64 nrows = SPI_processed;
            has_data = true;

            appendStringInfoChar(&buf, '[');

            for (uint64 i = 0; i < nrows; i++)
            {
                bool first = true;

                if (i > 0)
                    appendStringInfoChar(&buf, ',');

                appendStringInfoChar(&buf, '{');
                json_append_num(&buf, "pid",                i, 0, &first);
                json_append_str(&buf, "usename",            i, 1, &first);
                json_append_str(&buf, "application_name",   i, 2, &first);
                json_append_str(&buf, "client_addr",        i, 3, &first);
                json_append_str(&buf, "state",              i, 4, &first);
                json_append_str(&buf, "sent_lsn",           i, 5, &first);
                json_append_str(&buf, "write_lsn",          i, 6, &first);
                json_append_str(&buf, "flush_lsn",          i, 7, &first);
                json_append_str(&buf, "replay_lsn",         i, 8, &first);
                json_append_str(&buf, "write_lag",          i, 9, &first);
                json_append_str(&buf, "flush_lag",          i, 10, &first);
                json_append_str(&buf, "replay_lag",         i, 11, &first);
                appendStringInfoChar(&buf, '}');
            }

            appendStringInfoChar(&buf, ']');
        }
        else
        {
            appendStringInfoString(&buf, "[]");
        }

        /* --- pg_replication_slots --- */
        appendStringInfoString(&buf, ",\"slots\":");

        ret = sage_spi_exec(
            "SELECT slot_name, slot_type, active, "
            "       pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn) as lag_bytes "
            "FROM pg_replication_slots",
            0);

        if (ret >= 0 && SPI_processed > 0)
        {
            uint64 nrows = SPI_processed;
            has_data = true;

            appendStringInfoChar(&buf, '[');

            for (uint64 i = 0; i < nrows; i++)
            {
                bool first = true;

                if (i > 0)
                    appendStringInfoChar(&buf, ',');

                appendStringInfoChar(&buf, '{');
                json_append_str(&buf, "slot_name",  i, 0, &first);
                json_append_str(&buf, "slot_type",  i, 1, &first);
                json_append_str(&buf, "active",     i, 2, &first);
                json_append_num(&buf, "lag_bytes",  i, 3, &first);
                appendStringInfoChar(&buf, '}');
            }

            appendStringInfoChar(&buf, ']');
        }
        else
        {
            appendStringInfoString(&buf, "[]");
        }

        appendStringInfoChar(&buf, '}');

        if (has_data)
            insert_snapshot("replication", buf.data);

        pfree(buf.data);
    }
    PG_CATCH();
    {
        EmitErrorReport();
        FlushErrorState();
        err = true;
        elog(WARNING, "pg_sage: failed to collect replication stats");
    }
    PG_END_TRY();

    SPI_finish();
    if (err)
        AbortCurrentTransaction();
    else
    {
        PopActiveSnapshot();
        CommitTransactionCommand();
    }
}

/* ----------------------------------------------------------------
 * sage_collector_main — background worker entry point
 * ---------------------------------------------------------------- */
PGDLLEXPORT void
sage_collector_main(Datum main_arg)
{
    int advisory_ret;
    char *dbname;

    /* Set up signal handlers */
    pqsignal(SIGTERM, sage_collector_sigterm);
    pqsignal(SIGHUP, sage_collector_sighup);

    /* Ready to receive signals */
    BackgroundWorkerUnblockSignals();

    /* Determine database name from bgw_extra */
    dbname = MyBgworkerEntry->bgw_extra;
    if (!dbname || dbname[0] == '\0')
        dbname = "postgres";

    /* Connect to the target database */
    BackgroundWorkerInitializeConnection(dbname, NULL, 0);

    elog(LOG, "pg_sage: collector worker started on database \"%s\"", dbname);

    /* Mark collector as running in shared state */
    LWLockAcquire(sage_state->lock, LW_EXCLUSIVE);
    sage_state->collector_running = true;
    LWLockRelease(sage_state->lock);

    /* Acquire advisory lock to prevent multiple instances */
    SetCurrentStatementStartTimestamp();
    StartTransactionCommand();
    PushActiveSnapshot(GetTransactionSnapshot());
    SPI_connect();
    advisory_ret = sage_spi_exec(
        "SELECT pg_try_advisory_lock(483722657)", 0);

    if (advisory_ret < 0 || SPI_processed == 0)
    {
        elog(WARNING, "pg_sage: collector could not check advisory lock, exiting");
        SPI_finish();
        PopActiveSnapshot();
        CommitTransactionCommand();
        goto cleanup;
    }

    {
        char *lock_result = sage_spi_getval_str(0, 0);
        if (lock_result == NULL || strcmp(lock_result, "t") != 0)
        {
            elog(WARNING, "pg_sage: another collector instance is already running, exiting");
            SPI_finish();
            PopActiveSnapshot();
            CommitTransactionCommand();
            goto cleanup;
        }
    }
    SPI_finish();
    PopActiveSnapshot();
    CommitTransactionCommand();

    elog(LOG, "pg_sage: collector acquired advisory lock");

    /* Main collection loop */
    while (!got_sigterm)
    {
        TimestampTz cycle_start;
        double      cycle_duration_ms;
        int         rc;

        /* Wait at the top of the loop (including first iteration) to
         * give the init scripts time to create the sage schema. */
        rc = WaitLatch(MyLatch,
                       WL_LATCH_SET | WL_TIMEOUT | WL_POSTMASTER_DEATH,
                       sage_collector_interval * 1000L,
                       PG_WAIT_EXTENSION);
        ResetLatch(MyLatch);
        if (rc & WL_POSTMASTER_DEATH)
            break;

        /* Handle SIGHUP: reload config */
        if (got_sighup)
        {
            got_sighup = false;
            ProcessConfigFile(PGC_SIGHUP);
            elog(LOG, "pg_sage: collector reloaded configuration");
        }

        CHECK_FOR_INTERRUPTS();

        /* Check if sage is enabled */
        if (!sage_enabled)
        {
            elog(DEBUG1, "pg_sage: collector disabled, waiting...");
            rc = WaitLatch(MyLatch,
                           WL_LATCH_SET | WL_TIMEOUT | WL_POSTMASTER_DEATH,
                           sage_collector_interval * 1000L,
                           PG_WAIT_EXTENSION);
            ResetLatch(MyLatch);
            if (rc & WL_POSTMASTER_DEATH)
                break;
            continue;
        }

        /* Check circuit breaker */
        if (!sage_circuit_check())
        {
            elog(DEBUG1, "pg_sage: circuit breaker tripped, backing off");
            sage_circuit_record_skip();

            /* Wait longer when circuit is open */
            rc = WaitLatch(MyLatch,
                           WL_LATCH_SET | WL_TIMEOUT | WL_POSTMASTER_DEATH,
                           sage_collector_interval * 3000L,
                           PG_WAIT_EXTENSION);
            ResetLatch(MyLatch);
            if (rc & WL_POSTMASTER_DEATH)
                break;
            continue;
        }

        /* Check emergency stop */
        LWLockAcquire(sage_state->lock, LW_SHARED);
        if (sage_state->emergency_stopped)
        {
            LWLockRelease(sage_state->lock);
            elog(DEBUG1, "pg_sage: emergency stopped, waiting...");
            rc = WaitLatch(MyLatch,
                           WL_LATCH_SET | WL_TIMEOUT | WL_POSTMASTER_DEATH,
                           sage_collector_interval * 1000L,
                           PG_WAIT_EXTENSION);
            ResetLatch(MyLatch);
            if (rc & WL_POSTMASTER_DEATH)
                break;
            continue;
        }
        LWLockRelease(sage_state->lock);

        /* Begin collection cycle */
        cycle_start = GetCurrentTimestamp();

        elog(DEBUG1, "pg_sage: starting collection cycle");

        /* Run all collection functions */
        sage_collect_stat_statements();
        if (got_sigterm) break;

        sage_collect_table_stats();
        if (got_sigterm) break;

        sage_collect_index_stats();
        if (got_sigterm) break;

        sage_collect_system_stats();
        if (got_sigterm) break;

        sage_collect_lock_stats();
        if (got_sigterm) break;

        sage_collect_sequence_stats();
        if (got_sigterm) break;

        sage_collect_replication_stats();
        if (got_sigterm) break;

        sage_self_monitor_collect();
        if (got_sigterm) break;

        /* Drain auto-explain queue (passive EXPLAIN capture) */
        sage_autoexplain_drain_queue();
        if (got_sigterm) break;

        /* Record cycle timing in shared state */
        cycle_duration_ms = (double)(GetCurrentTimestamp() - cycle_start) / 1000.0;

        LWLockAcquire(sage_state->lock, LW_EXCLUSIVE);
        sage_state->last_collect_time = GetCurrentTimestamp();
        sage_state->last_collector_duration_ms = cycle_duration_ms;
        LWLockRelease(sage_state->lock);

        sage_circuit_record_success();

        elog(DEBUG1, "pg_sage: collection cycle completed in %.2f ms", cycle_duration_ms);
    }

    /* Release advisory lock */
    SetCurrentStatementStartTimestamp();
    StartTransactionCommand();
    PushActiveSnapshot(GetTransactionSnapshot());
    SPI_connect();
    sage_spi_exec("SELECT pg_advisory_unlock(483722657)", 0);
    SPI_finish();
    PopActiveSnapshot();
    CommitTransactionCommand();

    elog(LOG, "pg_sage: collector released advisory lock");

cleanup:
    /* Mark collector as stopped */
    LWLockAcquire(sage_state->lock, LW_EXCLUSIVE);
    sage_state->collector_running = false;
    LWLockRelease(sage_state->lock);

    elog(LOG, "pg_sage: collector worker exiting");
    proc_exit(0);
}
