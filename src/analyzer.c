/*
 * analyzer.c — The pg_sage rules engine / analyzer background worker
 *
 * Processes snapshots and live catalog data to generate findings
 * across all tiers (1-3). Runs analysis for unused indexes,
 * duplicate indexes, missing indexes, slow queries, query
 * regressions, sequential scans, sequence exhaustion, configuration
 * audit, index bloat, index write penalties, HA state, and
 * retention cleanup.
 *
 * AGPL-3.0 License
 */

#include "pg_sage.h"

#include <math.h>
#include <string.h>

#include "access/xact.h"
#include "executor/spi.h"
#include "pgstat.h"
#include "postmaster/bgworker.h"
#include "storage/ipc.h"
#include "storage/latch.h"
#include "storage/proc.h"
#include "utils/builtins.h"
#include "utils/memutils.h"
#include "utils/snapmgr.h"
#include "utils/timestamp.h"

/* Database name (defined in guc.c) */
extern char *sage_database;

/* Signal flags */
static volatile sig_atomic_t got_sighup = false;
static volatile sig_atomic_t got_sigterm = false;

/* ----------------------------------------------------------------
 * Signal handlers
 * ---------------------------------------------------------------- */
static void
sage_analyzer_sighup(SIGNAL_ARGS)
{
    int save_errno = errno;

    got_sighup = true;
    SetLatch(MyLatch);

    errno = save_errno;
}

static void
sage_analyzer_sigterm(SIGNAL_ARGS)
{
    int save_errno = errno;

    got_sigterm = true;
    SetLatch(MyLatch);

    errno = save_errno;
}

/* ----------------------------------------------------------------
 * Workload-adaptive scheduling
 *
 * Adjusts the analyzer interval based on database transaction rate.
 * High activity → more frequent analysis (down to 1/3 base interval).
 * Low activity → less frequent (up to 3x base interval).
 * Auto-calibrates baseline from the first 5 samples.
 * ---------------------------------------------------------------- */
#define ADAPTIVE_BASELINE_SAMPLES  5
#define ADAPTIVE_MIN_MULTIPLIER    0.33
#define ADAPTIVE_MAX_MULTIPLIER    3.0
#define ADAPTIVE_MIN_INTERVAL_MS   30000   /* 30 seconds floor */

static int
compute_adaptive_interval(void)
{
    int64       current_xact;
    TimestampTz now_ts;
    double      elapsed_sec;
    double      rate;
    double      multiplier;
    int         interval_ms;
    int         ret;

    if (!sage_state)
        return sage_analyzer_interval * 1000;

    /* Query current xact_commit from pg_stat_database */
    SPI_connect();
    ret = SPI_execute(
        "SELECT xact_commit FROM pg_stat_database "
        "WHERE datname = current_database()",
        true, 1);

    if (ret != SPI_OK_SELECT || SPI_processed == 0)
    {
        SPI_finish();
        return sage_analyzer_interval * 1000;
    }

    current_xact = sage_spi_getval_int64(0, 0);
    SPI_finish();

    now_ts = GetCurrentTimestamp();

    LWLockAcquire(sage_state->lock, LW_EXCLUSIVE);

    /* First sample: just store and return default */
    if (sage_state->last_xact_commit == 0)
    {
        sage_state->last_xact_commit = current_xact;
        sage_state->last_xact_sample_time = now_ts;
        sage_state->adaptive_interval_ms = sage_analyzer_interval * 1000;
        LWLockRelease(sage_state->lock);
        return sage_analyzer_interval * 1000;
    }

    /* Compute rate */
    elapsed_sec = (double)(now_ts - sage_state->last_xact_sample_time) / 1000000.0;
    if (elapsed_sec < 1.0)
        elapsed_sec = 1.0;

    rate = (double)(current_xact - sage_state->last_xact_commit) / elapsed_sec;
    if (rate < 0)
        rate = 0;

    sage_state->xact_rate_per_sec = rate;
    sage_state->last_xact_commit = current_xact;
    sage_state->last_xact_sample_time = now_ts;

    /* Auto-calibrate baseline from first N samples */
    if (sage_state->baseline_samples < ADAPTIVE_BASELINE_SAMPLES)
    {
        sage_state->baseline_samples++;
        if (sage_state->baseline_samples == 1)
            sage_state->baseline_xact_rate = rate;
        else
        {
            /* Running average */
            sage_state->baseline_xact_rate =
                sage_state->baseline_xact_rate +
                (rate - sage_state->baseline_xact_rate) / sage_state->baseline_samples;
        }
        sage_state->adaptive_interval_ms = sage_analyzer_interval * 1000;
        LWLockRelease(sage_state->lock);
        return sage_analyzer_interval * 1000;
    }

    /* Compute adaptive multiplier: higher rate → lower multiplier → shorter interval */
    if (sage_state->baseline_xact_rate > 0)
        multiplier = sage_state->baseline_xact_rate / Max(rate, 0.1);
    else
        multiplier = 1.0;

    /* Clamp */
    if (multiplier < ADAPTIVE_MIN_MULTIPLIER)
        multiplier = ADAPTIVE_MIN_MULTIPLIER;
    if (multiplier > ADAPTIVE_MAX_MULTIPLIER)
        multiplier = ADAPTIVE_MAX_MULTIPLIER;

    interval_ms = (int)(sage_analyzer_interval * 1000 * multiplier);
    if (interval_ms < ADAPTIVE_MIN_INTERVAL_MS)
        interval_ms = ADAPTIVE_MIN_INTERVAL_MS;

    sage_state->adaptive_interval_ms = interval_ms;

    LWLockRelease(sage_state->lock);

    ereport(DEBUG1,
            (errmsg("pg_sage adaptive scheduler: rate=%.1f tps, baseline=%.1f, "
                    "multiplier=%.2f, interval=%d ms",
                    rate, sage_state->baseline_xact_rate, multiplier, interval_ms)));

    return interval_ms;
}

/* ----------------------------------------------------------------
 * Helper: run a single analysis function inside PG_TRY/PG_CATCH,
 * measuring elapsed time.  Returns elapsed milliseconds.
 * ---------------------------------------------------------------- */
static double
run_analysis(const char *name, void (*fn)(void))
{
    instr_time  start, end;
    double      elapsed_ms;

    INSTR_TIME_SET_CURRENT(start);

    PG_TRY();
    {
        fn();
    }
    PG_CATCH();
    {
        ErrorData  *edata;

        /* Capture and log the error details before flushing */
        edata = CopyErrorData();
        FlushErrorState();

        ereport(LOG,
                (errmsg("pg_sage analyzer: analysis function \"%s\" failed: %s",
                        name, edata->message ? edata->message : "(no message)"),
                 errdetail("%s", edata->detail ? edata->detail : "")));
        FreeErrorData(edata);

        /*
         * After catching an error the current transaction is in aborted
         * state.  We must roll it back and start a fresh one so that the
         * caller's subsequent PopActiveSnapshot / CommitTransactionCommand
         * operate on a clean transaction.
         */
        AbortCurrentTransaction();
        StartTransactionCommand();
        PushActiveSnapshot(GetTransactionSnapshot());
    }
    PG_END_TRY();

    INSTR_TIME_SET_CURRENT(end);
    INSTR_TIME_SUBTRACT(end, start);
    elapsed_ms = INSTR_TIME_GET_MILLISEC(end);

    ereport(DEBUG1,
            (errmsg("pg_sage analyzer: \"%s\" completed in %.1f ms",
                    name, elapsed_ms)));

    return elapsed_ms;
}

/* ================================================================
 * Analysis functions
 * ================================================================ */

/* ----------------------------------------------------------------
 * sage_analyze_unused_indexes
 * ---------------------------------------------------------------- */
void
sage_analyze_unused_indexes(void)
{
    StringInfoData sql;
    int             ret;
    int             i;
    int             days;

    days = sage_parse_interval_days(sage_unused_index_window);
    if (days <= 0)
        days = 30;

    SPI_connect();

    /* Set a safety timeout for this analysis */
    SPI_exec("SET LOCAL statement_timeout = '500ms'", 0);

    initStringInfo(&sql);
    appendStringInfo(&sql,
        "WITH current_snap AS ( "
        "    SELECT (elem->>'indexrelname') as indexrelname, "
        "           (elem->>'schemaname') as schemaname, "
        "           (elem->>'relname') as relname, "
        "           (elem->>'idx_scan')::bigint as idx_scan, "
        "           (elem->>'index_bytes')::bigint as index_bytes "
        "    FROM sage.snapshots s, "
        "         jsonb_array_elements(s.data) elem "
        "    WHERE s.category = 'indexes' "
        "      AND s.collected_at > now() - interval '%d days' "
        "), "
        "agg AS ( "
        "    SELECT indexrelname, schemaname, relname, "
        "           sum(idx_scan) as total_scans, "
        "           max(index_bytes) as size_bytes "
        "    FROM current_snap "
        "    GROUP BY indexrelname, schemaname, relname "
        ") "
        "SELECT indexrelname, schemaname, relname, "
        "       total_scans, size_bytes "
        "FROM agg WHERE total_scans = 0 "
        "ORDER BY size_bytes DESC "
        "LIMIT 50",
        days);

    ret = SPI_execute(sql.data, true, 0);
    pfree(sql.data);

    if (ret == SPI_OK_SELECT && SPI_processed > 0)
    {
        for (i = 0; i < (int) SPI_processed; i++)
        {
            char   *indexrelname = pstrdup(sage_spi_getval_str(i, 0));
            char   *schemaname  = pstrdup(sage_spi_getval_str(i, 1));
            char   *relname     = pstrdup(sage_spi_getval_str(i, 2));
            int64   size_bytes  = sage_spi_getval_int64(i, 4);

            StringInfoData detail;
            StringInfoData rec;
            StringInfoData drop_sql;
            StringInfoData rollback_sql_buf;
            StringInfoData object_id;
            StringInfoData title_buf;

            initStringInfo(&detail);
            initStringInfo(&rec);
            initStringInfo(&drop_sql);
            initStringInfo(&rollback_sql_buf);
            initStringInfo(&object_id);
            initStringInfo(&title_buf);

            appendStringInfo(&object_id, "%s.%s", schemaname, indexrelname);

            appendStringInfo(&title_buf,
                "Unused index %s.%s on %s.%s (zero scans in %d days)",
                schemaname, indexrelname, schemaname, relname, days);

            {
                char *idx_full = psprintf("%s.%s", schemaname, indexrelname);
                char *tbl_full = psprintf("%s.%s", schemaname, relname);
                char *detail_json = sage_format_jsonb_object(
                    "index", idx_full,
                    "table", tbl_full,
                    "scans", "0",
                    "size_bytes", psprintf(INT64_FORMAT, size_bytes),
                    "window_days", psprintf("%d", days),
                    NULL);
                appendStringInfoString(&detail, detail_json);
                pfree(detail_json);
                pfree(idx_full);
                pfree(tbl_full);
            }

            appendStringInfo(&rec,
                "This index has had zero scans over the past %d days and "
                "consumes %s of disk. Consider dropping it to reclaim space "
                "and reduce write overhead.",
                days,
                size_bytes > 1073741824
                    ? psprintf("%.1f GB", (double) size_bytes / 1073741824.0)
                    : psprintf("%.1f MB", (double) size_bytes / 1048576.0));

            appendStringInfo(&drop_sql,
                "DROP INDEX CONCURRENTLY %s.%s;",
                schemaname, indexrelname);

            /* We cannot reconstruct CREATE INDEX here exactly, but
             * we can look up the definition from pg_indexes. For now,
             * store a placeholder that the executor/briefing can
             * resolve by querying pg_indexes. */
            appendStringInfo(&rollback_sql_buf,
                "-- Retrieve original DDL: "
                "SELECT indexdef FROM pg_indexes "
                "WHERE schemaname = '%s' AND indexname = '%s';",
                schemaname, indexrelname);

            sage_upsert_finding(
                "unused_index",
                size_bytes > 104857600 ? "critical" : "warning",  /* >100MB = critical */
                "index",
                object_id.data,
                title_buf.data,
                detail.data,
                rec.data,
                drop_sql.data,
                rollback_sql_buf.data);

            pfree(detail.data);
            pfree(rec.data);
            pfree(drop_sql.data);
            pfree(rollback_sql_buf.data);
            pfree(object_id.data);
            pfree(title_buf.data);
            pfree(indexrelname);
            pfree(schemaname);
            pfree(relname);
        }
    }

    SPI_finish();
}

/* ----------------------------------------------------------------
 * sage_analyze_duplicate_indexes
 *
 * Parse column lists from indexdef and flag duplicates/subsets on
 * the same table.
 * ---------------------------------------------------------------- */

/* Small struct to hold parsed index info */
typedef struct IdxInfo
{
    char   *schemaname;
    char   *tablename;
    char   *indexname;
    char   *indexdef;
    /* extracted column string from between the parentheses */
    char   *columns;
} IdxInfo;

/*
 * Extract the column-list portion from an indexdef string.
 * e.g.  "CREATE INDEX foo ON public.bar USING btree (a, b)" -> "a, b"
 * Returns a palloc'd string, or NULL.
 */
static char *
extract_index_columns(const char *indexdef)
{
    const char *open_paren;
    const char *close_paren;
    int         len;
    char       *result;

    if (!indexdef)
        return NULL;

    /* Find the LAST '(' ... ')' pair (handles WHERE clauses, etc.) */
    open_paren = strrchr(indexdef, '(');
    if (!open_paren)
        return NULL;

    /* Actually, we want the first '(' after the USING clause or
     * the first '(' that follows the table name. Let's find the
     * first '(' instead. */
    open_paren = strchr(indexdef, '(');
    if (!open_paren)
        return NULL;

    close_paren = strchr(open_paren, ')');
    if (!close_paren)
        return NULL;

    len = close_paren - open_paren - 1;
    if (len <= 0)
        return NULL;

    result = palloc(len + 1);
    memcpy(result, open_paren + 1, len);
    result[len] = '\0';

    return result;
}

/*
 * Check whether col_a is a subset or equal to col_b.
 * We split by comma, trim, and compare token-by-token.
 * Returns: 0 = no match, 1 = exact match, 2 = a is a prefix subset of b
 */
static int
compare_column_lists(const char *col_a, const char *col_b)
{
    char       *buf_a, *buf_b;
    char       *tokens_a[64], *tokens_b[64];
    int         na = 0, nb = 0;
    char       *tok;
    int         i;

    if (!col_a || !col_b)
        return 0;

    /* Tokenize a */
    buf_a = pstrdup(col_a);
    tok = strtok(buf_a, ",");
    while (tok && na < 64)
    {
        /* trim leading spaces */
        while (*tok == ' ') tok++;
        /* trim trailing spaces */
        {
            char *end = tok + strlen(tok) - 1;
            while (end > tok && *end == ' ') *end-- = '\0';
        }
        tokens_a[na++] = tok;
        tok = strtok(NULL, ",");
    }

    /* Tokenize b */
    buf_b = pstrdup(col_b);
    tok = strtok(buf_b, ",");
    while (tok && nb < 64)
    {
        while (*tok == ' ') tok++;
        {
            char *end = tok + strlen(tok) - 1;
            while (end > tok && *end == ' ') *end-- = '\0';
        }
        tokens_b[nb++] = tok;
        tok = strtok(NULL, ",");
    }

    if (na == 0 || nb == 0)
    {
        pfree(buf_a);
        pfree(buf_b);
        return 0;
    }

    /* Exact match check */
    if (na == nb)
    {
        bool match = true;

        for (i = 0; i < na; i++)
        {
            if (pg_strcasecmp(tokens_a[i], tokens_b[i]) != 0)
            {
                match = false;
                break;
            }
        }
        pfree(buf_a);
        pfree(buf_b);
        return match ? 1 : 0;
    }

    /* Prefix subset: a is a prefix of b (a has fewer columns, all match
     * leading columns of b in order) */
    if (na < nb)
    {
        bool match = true;

        for (i = 0; i < na; i++)
        {
            if (pg_strcasecmp(tokens_a[i], tokens_b[i]) != 0)
            {
                match = false;
                break;
            }
        }
        pfree(buf_a);
        pfree(buf_b);
        return match ? 2 : 0;
    }

    pfree(buf_a);
    pfree(buf_b);
    return 0;
}

void
sage_analyze_duplicate_indexes(void)
{
    int         ret;
    int         nidx;
    IdxInfo    *indexes;
    int         i, j;

    SPI_connect();
    SPI_exec("SET LOCAL statement_timeout = '500ms'", 0);

    ret = SPI_execute(
        "SELECT schemaname, tablename, indexname, indexdef "
        "FROM pg_indexes "
        "WHERE schemaname NOT IN ('pg_catalog', 'information_schema', 'sage') "
        "ORDER BY schemaname, tablename",
        true, 0);

    if (ret != SPI_OK_SELECT || SPI_processed == 0)
    {
        SPI_finish();
        return;
    }

    nidx = (int) SPI_processed;

    /* Copy results into local memory since SPI memory goes away */
    indexes = palloc(sizeof(IdxInfo) * nidx);
    for (i = 0; i < nidx; i++)
    {
        indexes[i].schemaname = pstrdup(sage_spi_getval_str(i, 0));
        indexes[i].tablename  = pstrdup(sage_spi_getval_str(i, 1));
        indexes[i].indexname  = pstrdup(sage_spi_getval_str(i, 2));
        indexes[i].indexdef   = pstrdup(sage_spi_getval_str(i, 3));
        indexes[i].columns    = extract_index_columns(indexes[i].indexdef);
    }

    /* Compare pairs on the same table */
    for (i = 0; i < nidx; i++)
    {
        for (j = i + 1; j < nidx; j++)
        {
            int cmp;

            CHECK_FOR_INTERRUPTS();

            /* Must be on the same table */
            if (strcmp(indexes[i].schemaname, indexes[j].schemaname) != 0 ||
                strcmp(indexes[i].tablename, indexes[j].tablename) != 0)
                continue;

            if (!indexes[i].columns || !indexes[j].columns)
                continue;

            cmp = compare_column_lists(indexes[i].columns, indexes[j].columns);
            if (cmp == 0)
            {
                /* Try the reverse: j is subset of i */
                cmp = compare_column_lists(indexes[j].columns, indexes[i].columns);
                if (cmp == 2)
                {
                    /* j is a strict prefix subset of i — flag j as redundant */
                    StringInfoData detail, object_id, title_buf, rec, drop_sql, rollback_buf;

                    initStringInfo(&detail);
                    initStringInfo(&object_id);
                    initStringInfo(&title_buf);
                    initStringInfo(&rec);
                    initStringInfo(&drop_sql);
                    initStringInfo(&rollback_buf);

                    appendStringInfo(&object_id, "%s.%s",
                                     indexes[j].schemaname, indexes[j].indexname);
                    appendStringInfo(&title_buf,
                        "Index %s.%s is a subset of %s.%s on %s.%s",
                        indexes[j].schemaname, indexes[j].indexname,
                        indexes[i].schemaname, indexes[i].indexname,
                        indexes[i].schemaname, indexes[i].tablename);
                    {
                        char *detail_json = sage_format_jsonb_object(
                            "subset_index", psprintf("%s.%s", indexes[j].schemaname, indexes[j].indexname),
                            "superset_index", psprintf("%s.%s", indexes[i].schemaname, indexes[i].indexname),
                            "table", psprintf("%s.%s", indexes[i].schemaname, indexes[i].tablename),
                            "subset_columns", indexes[j].columns,
                            "superset_columns", indexes[i].columns,
                            "type", "subset",
                            NULL);
                        appendStringInfoString(&detail, detail_json);
                        pfree(detail_json);
                    }
                    appendStringInfo(&rec,
                        "Index %s.%s has columns (%s) which are a leading "
                        "prefix of %s.%s (%s). The larger index can serve "
                        "queries on those leading columns. Consider dropping "
                        "the smaller index.",
                        indexes[j].schemaname, indexes[j].indexname,
                        indexes[j].columns,
                        indexes[i].schemaname, indexes[i].indexname,
                        indexes[i].columns);
                    appendStringInfo(&drop_sql,
                        "DROP INDEX CONCURRENTLY %s.%s;",
                        indexes[j].schemaname, indexes[j].indexname);
                    appendStringInfo(&rollback_buf, "%s", indexes[j].indexdef);

                    sage_upsert_finding("duplicate_index", "warning",
                                        "index", object_id.data,
                                        title_buf.data, detail.data,
                                        rec.data, drop_sql.data,
                                        rollback_buf.data);

                    pfree(detail.data);
                    pfree(object_id.data);
                    pfree(title_buf.data);
                    pfree(rec.data);
                    pfree(drop_sql.data);
                    pfree(rollback_buf.data);
                }
                continue;
            }

            if (cmp == 1)
            {
                /* Exact duplicate columns — flag the second one */
                StringInfoData detail, object_id, title_buf, rec, drop_sql, rollback_buf;

                initStringInfo(&detail);
                initStringInfo(&object_id);
                initStringInfo(&title_buf);
                initStringInfo(&rec);
                initStringInfo(&drop_sql);
                initStringInfo(&rollback_buf);

                appendStringInfo(&object_id, "%s.%s",
                                 indexes[j].schemaname, indexes[j].indexname);
                appendStringInfo(&title_buf,
                    "Duplicate index %s.%s matches %s.%s on %s.%s",
                    indexes[j].schemaname, indexes[j].indexname,
                    indexes[i].schemaname, indexes[i].indexname,
                    indexes[i].schemaname, indexes[i].tablename);
                {
                    char *detail_json = sage_format_jsonb_object(
                        "index_a", psprintf("%s.%s", indexes[i].schemaname, indexes[i].indexname),
                        "index_b", psprintf("%s.%s", indexes[j].schemaname, indexes[j].indexname),
                        "table", psprintf("%s.%s", indexes[i].schemaname, indexes[i].tablename),
                        "columns", indexes[i].columns,
                        "type", "exact_duplicate",
                        NULL);
                    appendStringInfoString(&detail, detail_json);
                    pfree(detail_json);
                }
                appendStringInfo(&rec,
                    "Indexes %s.%s and %s.%s on table %s.%s have identical "
                    "column definitions (%s). One of them can be safely "
                    "dropped to reclaim space and reduce write overhead.",
                    indexes[i].schemaname, indexes[i].indexname,
                    indexes[j].schemaname, indexes[j].indexname,
                    indexes[i].schemaname, indexes[i].tablename,
                    indexes[i].columns);
                appendStringInfo(&drop_sql,
                    "DROP INDEX CONCURRENTLY %s.%s;",
                    indexes[j].schemaname, indexes[j].indexname);
                appendStringInfo(&rollback_buf, "%s;", indexes[j].indexdef);

                sage_upsert_finding("duplicate_index", "critical",
                                    "index", object_id.data,
                                    title_buf.data, detail.data,
                                    rec.data, drop_sql.data,
                                    rollback_buf.data);

                pfree(detail.data);
                pfree(object_id.data);
                pfree(title_buf.data);
                pfree(rec.data);
                pfree(drop_sql.data);
                pfree(rollback_buf.data);
            }
            else if (cmp == 2)
            {
                /* i is a strict prefix subset of j — flag i as redundant */
                StringInfoData detail, object_id, title_buf, rec, drop_sql, rollback_buf;

                initStringInfo(&detail);
                initStringInfo(&object_id);
                initStringInfo(&title_buf);
                initStringInfo(&rec);
                initStringInfo(&drop_sql);
                initStringInfo(&rollback_buf);

                appendStringInfo(&object_id, "%s.%s",
                                 indexes[i].schemaname, indexes[i].indexname);
                appendStringInfo(&title_buf,
                    "Index %s.%s is a subset of %s.%s on %s.%s",
                    indexes[i].schemaname, indexes[i].indexname,
                    indexes[j].schemaname, indexes[j].indexname,
                    indexes[j].schemaname, indexes[j].tablename);
                {
                    char *detail_json = sage_format_jsonb_object(
                        "subset_index", psprintf("%s.%s", indexes[i].schemaname, indexes[i].indexname),
                        "superset_index", psprintf("%s.%s", indexes[j].schemaname, indexes[j].indexname),
                        "table", psprintf("%s.%s", indexes[j].schemaname, indexes[j].tablename),
                        "subset_columns", indexes[i].columns,
                        "superset_columns", indexes[j].columns,
                        "type", "subset",
                        NULL);
                    appendStringInfoString(&detail, detail_json);
                    pfree(detail_json);
                }
                appendStringInfo(&rec,
                    "Index %s.%s has columns (%s) which are a leading "
                    "prefix of %s.%s (%s). The larger index can serve "
                    "queries on those leading columns. Consider dropping "
                    "the smaller index.",
                    indexes[i].schemaname, indexes[i].indexname,
                    indexes[i].columns,
                    indexes[j].schemaname, indexes[j].indexname,
                    indexes[j].columns);
                appendStringInfo(&drop_sql,
                    "DROP INDEX CONCURRENTLY %s.%s;",
                    indexes[i].schemaname, indexes[i].indexname);
                appendStringInfo(&rollback_buf, "%s;", indexes[i].indexdef);

                sage_upsert_finding("duplicate_index", "warning",
                                    "index", object_id.data,
                                    title_buf.data, detail.data,
                                    rec.data, drop_sql.data,
                                    rollback_buf.data);

                pfree(detail.data);
                pfree(object_id.data);
                pfree(title_buf.data);
                pfree(rec.data);
                pfree(drop_sql.data);
                pfree(rollback_buf.data);
            }
        }
    }

    for (i = 0; i < nidx; i++)
    {
        pfree(indexes[i].schemaname);
        pfree(indexes[i].tablename);
        pfree(indexes[i].indexname);
        pfree(indexes[i].indexdef);
        if (indexes[i].columns)
            pfree(indexes[i].columns);
    }
    pfree(indexes);

    SPI_finish();
}

/* ----------------------------------------------------------------
 * sage_analyze_missing_indexes
 * ---------------------------------------------------------------- */
void
sage_analyze_missing_indexes(void)
{
    StringInfoData  sql;
    int             ret_tables, ret_queries;
    int             ntables, nqueries;
    int             i, j;

    /* Temporary arrays to hold results across two SPI calls */
    typedef struct
    {
        char   *schemaname;
        char   *relname;
        int64   seq_scan;
        int64   seq_tup_read;
        int64   n_live_tup;
    } SeqScanTable;

    typedef struct
    {
        int64   queryid;
        char   *query;
        double  mean_exec_time;
        int64   calls;
    } SlowQuery;

    SeqScanTable   *tables = NULL;
    SlowQuery      *queries = NULL;

    SPI_connect();
    SPI_exec("SET LOCAL statement_timeout = '500ms'", 0);

    /* Step 1: large tables with sequential scans */
    initStringInfo(&sql);
    appendStringInfo(&sql,
        "SELECT schemaname, relname, seq_scan, seq_tup_read, n_live_tup "
        "FROM pg_stat_user_tables "
        "WHERE seq_scan > 0 AND n_live_tup > %d "
        "ORDER BY seq_tup_read DESC "
        "LIMIT 20",
        sage_seq_scan_min_rows);

    ret_tables = SPI_execute(sql.data, true, 0);
    pfree(sql.data);

    if (ret_tables != SPI_OK_SELECT || SPI_processed == 0)
    {
        SPI_finish();
        return;
    }

    ntables = (int) SPI_processed;
    tables = palloc(sizeof(SeqScanTable) * ntables);
    for (i = 0; i < ntables; i++)
    {
        tables[i].schemaname   = pstrdup(sage_spi_getval_str(i, 0));
        tables[i].relname      = pstrdup(sage_spi_getval_str(i, 1));
        tables[i].seq_scan     = sage_spi_getval_int64(i, 2);
        tables[i].seq_tup_read = sage_spi_getval_int64(i, 3);
        tables[i].n_live_tup   = sage_spi_getval_int64(i, 4);
    }

    /* Step 2: slow queries from pg_stat_statements */
    initStringInfo(&sql);
    appendStringInfo(&sql,
        "SELECT s.queryid, s.query, s.mean_exec_time, s.calls "
        "FROM pg_stat_statements s "
        "JOIN pg_database d ON d.oid = s.dbid "
        "WHERE d.datname = current_database() "
        "  AND s.mean_exec_time > %d "
        "ORDER BY s.total_exec_time DESC "
        "LIMIT 50",
        sage_slow_query_threshold);

    ret_queries = SPI_execute(sql.data, true, 0);
    pfree(sql.data);

    if (ret_queries != SPI_OK_SELECT || SPI_processed == 0)
    {
        for (i = 0; i < ntables; i++)
        {
            pfree(tables[i].schemaname);
            pfree(tables[i].relname);
        }
        pfree(tables);
        SPI_finish();
        return;
    }

    nqueries = (int) SPI_processed;
    queries = palloc(sizeof(SlowQuery) * nqueries);
    for (i = 0; i < nqueries; i++)
    {
        queries[i].queryid        = sage_spi_getval_int64(i, 0);
        queries[i].query          = pstrdup(sage_spi_getval_str(i, 1));
        queries[i].mean_exec_time = sage_spi_getval_float(i, 2);
        queries[i].calls          = sage_spi_getval_int64(i, 3);
    }

    /* Step 3: Match slow queries to tables by checking if table name
     * appears in the query text (case-insensitive). */
    for (i = 0; i < ntables; i++)
    {
        for (j = 0; j < nqueries; j++)
        {
            char   *lower_query;
            char   *lower_table;
            bool    found_table;

            /* Manual lowercase — str_tolower requires catalog access */
            {
                char *p;
                lower_query = pstrdup(queries[j].query);
                for (p = lower_query; *p; p++)
                    *p = pg_tolower((unsigned char) *p);
                lower_table = pstrdup(tables[i].relname);
                for (p = lower_table; *p; p++)
                    *p = pg_tolower((unsigned char) *p);
            }

            found_table = (strstr(lower_query, lower_table) != NULL);

            pfree(lower_query);
            pfree(lower_table);

            if (found_table)
            {
                StringInfoData detail, object_id, title_buf, rec;

                initStringInfo(&detail);
                initStringInfo(&object_id);
                initStringInfo(&title_buf);
                initStringInfo(&rec);

                appendStringInfo(&object_id, "%s.%s:queryid=" INT64_FORMAT,
                                 tables[i].schemaname, tables[i].relname,
                                 queries[j].queryid);

                appendStringInfo(&title_buf,
                    "Possible missing index on %s.%s (slow query %.0f ms, "
                    INT64_FORMAT " seq scans)",
                    tables[i].schemaname, tables[i].relname,
                    queries[j].mean_exec_time,
                    tables[i].seq_scan);

                {
                    char *query_preview = pnstrdup(queries[j].query, 200);
                    char *detail_json = sage_format_jsonb_object(
                        "table", psprintf("%s.%s", tables[i].schemaname, tables[i].relname),
                        "queryid", psprintf(INT64_FORMAT, queries[j].queryid),
                        "mean_exec_time_ms", psprintf("%.2f", queries[j].mean_exec_time),
                        "calls", psprintf(INT64_FORMAT, queries[j].calls),
                        "seq_scan", psprintf(INT64_FORMAT, tables[i].seq_scan),
                        "seq_tup_read", psprintf(INT64_FORMAT, tables[i].seq_tup_read),
                        "n_live_tup", psprintf(INT64_FORMAT, tables[i].n_live_tup),
                        "query_preview", query_preview,
                        NULL);
                    appendStringInfoString(&detail, detail_json);
                    pfree(detail_json);
                    pfree(query_preview);
                }

                appendStringInfo(&rec,
                    "Table %s.%s has " INT64_FORMAT " sequential scans and a "
                    "slow query (%.0f ms mean, " INT64_FORMAT " calls) "
                    "references it. Examine the query's WHERE and JOIN "
                    "clauses and consider adding an index on the filtered "
                    "columns. Use sage.explain(queryid) to inspect the plan.",
                    tables[i].schemaname, tables[i].relname,
                    tables[i].seq_scan,
                    queries[j].mean_exec_time, queries[j].calls);

                sage_upsert_finding("missing_index",
                    queries[j].mean_exec_time > 5000 ? "critical" : "warning",
                    "table", object_id.data,
                    title_buf.data, detail.data,
                    rec.data, NULL, NULL);

                pfree(detail.data);
                pfree(object_id.data);
                pfree(title_buf.data);
                pfree(rec.data);

                /* Only report one match per table to keep findings concise */
                break;
            }
        }
    }

    for (i = 0; i < ntables; i++)
    {
        pfree(tables[i].schemaname);
        pfree(tables[i].relname);
    }
    pfree(tables);

    for (i = 0; i < nqueries; i++)
        pfree(queries[i].query);
    pfree(queries);

    SPI_finish();
}

/* ----------------------------------------------------------------
 * sage_analyze_slow_queries
 * ---------------------------------------------------------------- */
void
sage_analyze_slow_queries(void)
{
    StringInfoData sql;
    int     ret;
    int     i;

    SPI_connect();
    SPI_exec("SET LOCAL statement_timeout = '500ms'", 0);

    initStringInfo(&sql);
    appendStringInfo(&sql,
        "SELECT s.queryid, s.query, s.calls, s.mean_exec_time, "
        "       s.total_exec_time, s.rows, "
        "       s.shared_blks_hit, s.shared_blks_read, "
        "       s.temp_blks_written "
        "FROM pg_stat_statements s "
        "JOIN pg_database d ON d.oid = s.dbid "
        "WHERE d.datname = current_database() "
        "  AND s.mean_exec_time > %d "
        "ORDER BY s.total_exec_time DESC "
        "LIMIT 20",
        sage_slow_query_threshold);

    ret = SPI_execute(sql.data, true, 0);
    pfree(sql.data);

    if (ret == SPI_OK_SELECT && SPI_processed > 0)
    {
        for (i = 0; i < (int) SPI_processed; i++)
        {
            int64   queryid         = sage_spi_getval_int64(i, 0);
            char   *query           = pstrdup(sage_spi_getval_str(i, 1));
            int64   calls           = sage_spi_getval_int64(i, 2);
            double  mean_exec_time  = sage_spi_getval_float(i, 3);
            double  total_exec_time = sage_spi_getval_float(i, 4);
            int64   rows            = sage_spi_getval_int64(i, 5);
            int64   shared_blks_hit = sage_spi_getval_int64(i, 6);
            int64   shared_blks_read = sage_spi_getval_int64(i, 7);
            int64   temp_blks_written = sage_spi_getval_int64(i, 8);

            StringInfoData detail, object_id, title_buf, rec;
            double  cache_hit_ratio;
            const char *severity;

            initStringInfo(&detail);
            initStringInfo(&object_id);
            initStringInfo(&title_buf);
            initStringInfo(&rec);

            cache_hit_ratio = (shared_blks_hit + shared_blks_read) > 0
                ? 100.0 * shared_blks_hit / (shared_blks_hit + shared_blks_read)
                : 100.0;

            appendStringInfo(&object_id, "queryid:" INT64_FORMAT, queryid);

            appendStringInfo(&title_buf,
                "Slow query (%.0f ms mean, " INT64_FORMAT " calls): %.80s...",
                mean_exec_time, calls, query);

            {
                char *query_truncated = pnstrdup(query, 500);
                char *detail_json = sage_format_jsonb_object(
                    "queryid", psprintf(INT64_FORMAT, queryid),
                    "mean_exec_time_ms", psprintf("%.2f", mean_exec_time),
                    "total_exec_time_ms", psprintf("%.2f", total_exec_time),
                    "calls", psprintf(INT64_FORMAT, calls),
                    "rows", psprintf(INT64_FORMAT, rows),
                    "shared_blks_hit", psprintf(INT64_FORMAT, shared_blks_hit),
                    "shared_blks_read", psprintf(INT64_FORMAT, shared_blks_read),
                    "temp_blks_written", psprintf(INT64_FORMAT, temp_blks_written),
                    "cache_hit_ratio", psprintf("%.2f", cache_hit_ratio),
                    "query", query_truncated,
                    NULL);
                appendStringInfoString(&detail, detail_json);
                pfree(detail_json);
                pfree(query_truncated);
            }

            if (mean_exec_time > 10000)
                severity = "critical";
            else if (mean_exec_time > 5000)
                severity = "warning";
            else
                severity = "info";

            appendStringInfo(&rec,
                "Query (id " INT64_FORMAT ") averages %.0f ms over "
                INT64_FORMAT " calls (total %.0f s). ",
                queryid, mean_exec_time, calls,
                total_exec_time / 1000.0);

            if (cache_hit_ratio < 95.0)
                appendStringInfo(&rec,
                    "Cache hit ratio is low (%.1f%%) — this query reads "
                    "heavily from disk. ",
                    cache_hit_ratio);

            if (temp_blks_written > 0)
                appendStringInfo(&rec,
                    "Temp blocks written (" INT64_FORMAT ") suggest "
                    "work_mem may be too low or a large sort/hash. ",
                    temp_blks_written);

            appendStringInfoString(&rec,
                "Run sage.explain(queryid) to capture the plan and "
                "identify optimisation opportunities.");

            sage_upsert_finding("slow_query", severity,
                                "query", object_id.data,
                                title_buf.data, detail.data,
                                rec.data, NULL, NULL);

            pfree(detail.data);
            pfree(object_id.data);
            pfree(title_buf.data);
            pfree(rec.data);
            pfree(query);
        }
    }

    SPI_finish();
}

/* ----------------------------------------------------------------
 * sage_analyze_query_regressions
 * ---------------------------------------------------------------- */
void
sage_analyze_query_regressions(void)
{
    int     ret;
    int     i;

    SPI_connect();
    SPI_exec("SET LOCAL statement_timeout = '500ms'", 0);

    ret = SPI_execute(
        "WITH current_stats AS ( "
        "    SELECT s.queryid, s.query, s.mean_exec_time, s.calls "
        "    FROM pg_stat_statements s "
        "    JOIN pg_database d ON d.oid = s.dbid "
        "    WHERE d.datname = current_database() "
        "), "
        "baseline AS ( "
        "    SELECT (elem->>'queryid')::bigint as queryid, "
        "           avg((elem->>'mean_exec_time')::float) as baseline_mean "
        "    FROM sage.snapshots s, "
        "         jsonb_array_elements(s.data) elem "
        "    WHERE s.category = 'queries' "
        "      AND s.collected_at BETWEEN now() - interval '7 days' "
        "                              AND now() - interval '1 day' "
        "    GROUP BY (elem->>'queryid')::bigint "
        ") "
        "SELECT c.queryid, c.query, c.mean_exec_time, b.baseline_mean, "
        "       ((c.mean_exec_time - b.baseline_mean) "
        "        / NULLIF(b.baseline_mean, 0) * 100) as pct_change "
        "FROM current_stats c "
        "JOIN baseline b ON c.queryid = b.queryid "
        "WHERE c.mean_exec_time > b.baseline_mean * 1.5 "
        "ORDER BY pct_change DESC "
        "LIMIT 10",
        true, 0);

    if (ret == SPI_OK_SELECT && SPI_processed > 0)
    {
        for (i = 0; i < (int) SPI_processed; i++)
        {
            int64   queryid        = sage_spi_getval_int64(i, 0);
            char   *query          = pstrdup(sage_spi_getval_str(i, 1));
            double  current_mean   = sage_spi_getval_float(i, 2);
            double  baseline_mean  = sage_spi_getval_float(i, 3);
            double  pct_change     = sage_spi_getval_float(i, 4);

            StringInfoData detail, object_id, title_buf, rec;
            const char *severity;

            initStringInfo(&detail);
            initStringInfo(&object_id);
            initStringInfo(&title_buf);
            initStringInfo(&rec);

            appendStringInfo(&object_id,
                             "regression:queryid:" INT64_FORMAT, queryid);

            appendStringInfo(&title_buf,
                "Query regression: %.0f%% slower (%.0f ms -> %.0f ms): "
                "%.60s...",
                pct_change, baseline_mean, current_mean, query);

            {
                char *query_truncated = pnstrdup(query, 500);
                char *detail_json = sage_format_jsonb_object(
                    "queryid", psprintf(INT64_FORMAT, queryid),
                    "current_mean_ms", psprintf("%.2f", current_mean),
                    "baseline_mean_ms", psprintf("%.2f", baseline_mean),
                    "pct_change", psprintf("%.1f", pct_change),
                    "query", query_truncated,
                    NULL);
                appendStringInfoString(&detail, detail_json);
                pfree(detail_json);
                pfree(query_truncated);
            }

            if (pct_change > 500.0)
                severity = "critical";
            else if (pct_change > 200.0)
                severity = "warning";
            else
                severity = "info";

            appendStringInfo(&rec,
                "Query (id " INT64_FORMAT ") has regressed by %.0f%% "
                "compared to the 7-day baseline (%.0f ms baseline -> "
                "%.0f ms now). Investigate recent schema changes, data "
                "growth, or plan regressions. "
                "Run sage.explain(queryid) to compare current plan.",
                queryid, pct_change, baseline_mean, current_mean);

            sage_upsert_finding("query_regression", severity,
                                "query", object_id.data,
                                title_buf.data, detail.data,
                                rec.data, NULL, NULL);

            pfree(detail.data);
            pfree(object_id.data);
            pfree(title_buf.data);
            pfree(rec.data);
            pfree(query);
        }
    }

    SPI_finish();
}

/* ----------------------------------------------------------------
 * sage_analyze_seq_scans
 * ---------------------------------------------------------------- */
void
sage_analyze_seq_scans(void)
{
    StringInfoData sql;
    int     ret;
    int     i;

    SPI_connect();
    SPI_exec("SET LOCAL statement_timeout = '500ms'", 0);

    initStringInfo(&sql);
    appendStringInfo(&sql,
        "SELECT schemaname, relname, seq_scan, idx_scan, n_live_tup, "
        "       pg_size_pretty(pg_total_relation_size(relid)) as size "
        "FROM pg_stat_user_tables "
        "WHERE n_live_tup > %d AND seq_scan > idx_scan AND seq_scan > 100 "
        "ORDER BY seq_tup_read DESC "
        "LIMIT 20",
        sage_seq_scan_min_rows);

    ret = SPI_execute(sql.data, true, 0);
    pfree(sql.data);

    if (ret == SPI_OK_SELECT && SPI_processed > 0)
    {
        for (i = 0; i < (int) SPI_processed; i++)
        {
            char   *schemaname = pstrdup(sage_spi_getval_str(i, 0));
            char   *relname    = pstrdup(sage_spi_getval_str(i, 1));
            int64   seq_scan   = sage_spi_getval_int64(i, 2);
            int64   idx_scan   = sage_spi_getval_int64(i, 3);
            int64   n_live_tup = sage_spi_getval_int64(i, 4);
            char   *size       = pstrdup(sage_spi_getval_str(i, 5));

            StringInfoData detail, object_id, title_buf, rec;
            double  ratio;

            initStringInfo(&detail);
            initStringInfo(&object_id);
            initStringInfo(&title_buf);
            initStringInfo(&rec);

            ratio = idx_scan > 0
                ? (double) seq_scan / (double) idx_scan
                : (double) seq_scan;

            appendStringInfo(&object_id, "%s.%s", schemaname, relname);

            appendStringInfo(&title_buf,
                "High sequential scan ratio on %s.%s "
                "(" INT64_FORMAT " seq vs " INT64_FORMAT " idx scans, "
                INT64_FORMAT " rows, %s)",
                schemaname, relname, seq_scan, idx_scan, n_live_tup, size);

            {
                char *detail_json = sage_format_jsonb_object(
                    "table", psprintf("%s.%s", schemaname, relname),
                    "seq_scan", psprintf(INT64_FORMAT, seq_scan),
                    "idx_scan", psprintf(INT64_FORMAT, idx_scan),
                    "n_live_tup", psprintf(INT64_FORMAT, n_live_tup),
                    "size", size,
                    "seq_to_idx_ratio", psprintf("%.1f", ratio),
                    NULL);
                appendStringInfoString(&detail, detail_json);
                pfree(detail_json);
            }

            appendStringInfo(&rec,
                "Table %s.%s (%s, " INT64_FORMAT " rows) has "
                INT64_FORMAT " sequential scans vs only " INT64_FORMAT
                " index scans (ratio %.1f:1). This suggests missing or "
                "underused indexes. Check queries against this table and "
                "add indexes on frequently filtered columns.",
                schemaname, relname, size, n_live_tup,
                seq_scan, idx_scan, ratio);

            sage_upsert_finding("seq_scan_heavy",
                ratio > 100.0 ? "critical" : "warning",
                "table", object_id.data,
                title_buf.data, detail.data,
                rec.data, NULL, NULL);

            pfree(detail.data);
            pfree(object_id.data);
            pfree(title_buf.data);
            pfree(rec.data);
            pfree(schemaname);
            pfree(relname);
            pfree(size);
        }
    }

    SPI_finish();
}

/* ----------------------------------------------------------------
 * sage_analyze_sequence_exhaustion
 * ---------------------------------------------------------------- */
void
sage_analyze_sequence_exhaustion(void)
{
    int     ret;
    int     i;

    SPI_connect();
    SPI_exec("SET LOCAL statement_timeout = '500ms'", 0);

    ret = SPI_execute(
        "SELECT schemaname, sequencename, last_value, max_value, data_type, "
        "       CASE WHEN max_value > 0 "
        "            THEN (last_value::float / max_value * 100) "
        "            ELSE 0 END as pct_used "
        "FROM pg_sequences "
        "WHERE last_value IS NOT NULL",
        true, 0);

    if (ret == SPI_OK_SELECT && SPI_processed > 0)
    {
        /* Save the outer result set — inner queries overwrite SPI_tuptable */
        SPITupleTable  *outer_tuptable = SPI_tuptable;
        uint64          outer_processed = SPI_processed;

        for (i = 0; i < (int) outer_processed; i++)
        {
            char   *schemaname   = pstrdup(sage_spi_getval_str(i, 0));
            char   *sequencename = pstrdup(sage_spi_getval_str(i, 1));
            int64   last_value   = sage_spi_getval_int64(i, 2);
            int64   max_value    = sage_spi_getval_int64(i, 3);
            char   *data_type    = pstrdup(sage_spi_getval_str(i, 4));
            double  pct_used     = sage_spi_getval_float(i, 5);

            const char *severity = NULL;
            bool        is_bigint;

            is_bigint = (pg_strcasecmp(data_type, "bigint") == 0);

            /* For BIGINT sequences, only alert if growth rate projects
             * exhaustion within 1 year. Estimate from snapshot history. */
            if (is_bigint && pct_used < 90.0)
            {
                /*
                 * Try to estimate growth rate from snapshot data.
                 * If we don't have enough history or the projected
                 * exhaustion is > 1 year away, skip the finding.
                 */
                StringInfoData growth_sql;
                int growth_ret;

                initStringInfo(&growth_sql);
                appendStringInfo(&growth_sql,
                    "WITH seq_history AS ( "
                    "    SELECT (elem->>'last_value')::bigint as lv, "
                    "           s.collected_at "
                    "    FROM sage.snapshots s, "
                    "         jsonb_array_elements(s.data) elem "
                    "    WHERE s.category = 'sequences' "
                    "      AND elem->>'sequencename' = '%s' "
                    "      AND elem->>'schemaname' = '%s' "
                    "    ORDER BY s.collected_at DESC "
                    "    LIMIT 2 "
                    ") "
                    "SELECT CASE WHEN count(*) >= 2 THEN "
                    "  (max(lv) - min(lv))::float / "
                    "  GREATEST(EXTRACT(EPOCH FROM (max(collected_at) - min(collected_at))), 1) "
                    "ELSE 0 END as rate_per_sec "
                    "FROM seq_history",
                    sequencename, schemaname);

                growth_ret = SPI_execute(growth_sql.data, true, 0);
                pfree(growth_sql.data);

                if (growth_ret == SPI_OK_SELECT && SPI_processed > 0)
                {
                    double rate_per_sec = sage_spi_getval_float(0, 0);

                    if (rate_per_sec > 0)
                    {
                        double remaining = (double)(max_value - last_value);
                        double seconds_to_exhaust = remaining / rate_per_sec;
                        double years_to_exhaust = seconds_to_exhaust /
                                                  (365.25 * 86400);

                        if (years_to_exhaust > 1.0)
                        {
                            /* Restore outer result set before continuing loop */
                            SPI_tuptable = outer_tuptable;
                            SPI_processed = outer_processed;
                            continue;  /* Not urgent for bigint */
                        }

                        /* Will exhaust within a year — fall through */
                    }
                    else
                    {
                        /* No measurable growth — skip */
                        if (pct_used < 75.0)
                        {
                            SPI_tuptable = outer_tuptable;
                            SPI_processed = outer_processed;
                            continue;
                        }
                    }
                }
                else
                {
                    if (pct_used < 75.0)
                    {
                        SPI_tuptable = outer_tuptable;
                        SPI_processed = outer_processed;
                        continue;
                    }
                }

                /* Restore outer result set after inner query */
                SPI_tuptable = outer_tuptable;
                SPI_processed = outer_processed;
            }

            /* Determine severity */
            if (pct_used >= 90.0)
                severity = "critical";
            else if (pct_used >= 75.0)
                severity = "warning";
            else
                continue;   /* Below threshold */

            {
                StringInfoData detail, object_id, title_buf, rec, rec_sql;

                initStringInfo(&detail);
                initStringInfo(&object_id);
                initStringInfo(&title_buf);
                initStringInfo(&rec);
                initStringInfo(&rec_sql);

                appendStringInfo(&object_id, "%s.%s",
                                 schemaname, sequencename);

                appendStringInfo(&title_buf,
                    "Sequence %s.%s at %.1f%% capacity (%s)",
                    schemaname, sequencename, pct_used, data_type);

                {
                    char *detail_json = sage_format_jsonb_object(
                        "sequence", psprintf("%s.%s", schemaname, sequencename),
                        "data_type", data_type,
                        "last_value", psprintf(INT64_FORMAT, last_value),
                        "max_value", psprintf(INT64_FORMAT, max_value),
                        "pct_used", psprintf("%.2f", pct_used),
                        NULL);
                    appendStringInfoString(&detail, detail_json);
                    pfree(detail_json);
                }

                if (!is_bigint)
                {
                    appendStringInfo(&rec,
                        "Sequence %s.%s (%s) is at %.1f%% of its maximum "
                        "value. Consider altering the sequence to use BIGINT "
                        "to prevent exhaustion and potential application "
                        "failures.",
                        schemaname, sequencename, data_type, pct_used);

                    appendStringInfo(&rec_sql,
                        "ALTER SEQUENCE %s.%s AS bigint;",
                        schemaname, sequencename);
                }
                else
                {
                    appendStringInfo(&rec,
                        "Sequence %s.%s (bigint) is at %.1f%% capacity. "
                        "At the current growth rate, it may be exhausted "
                        "within a year. Review usage patterns and consider "
                        "resetting or cycling the sequence.",
                        schemaname, sequencename, pct_used);
                }

                sage_upsert_finding("sequence_exhaustion", severity,
                                    "sequence", object_id.data,
                                    title_buf.data, detail.data,
                                    rec.data,
                                    rec_sql.len > 0 ? rec_sql.data : NULL,
                                    NULL);

                pfree(detail.data);
                pfree(object_id.data);
                pfree(title_buf.data);
                pfree(rec.data);
                pfree(rec_sql.data);
            }
        }
    }

    SPI_finish();
}

/* ----------------------------------------------------------------
 * sage_analyze_config
 * ---------------------------------------------------------------- */
void
sage_analyze_config(void)
{
    int     ret;
    int64   shared_buffers_bytes = 0;
    int64   effective_cache_size_bytes = 0;
    int64   work_mem_bytes = 0;
    int64   maintenance_work_mem_bytes = 0;
    int     max_connections_val = 0;
    double  checkpoint_target = 0.0;
    double  random_page_cost_val = 0.0;
    int64   total_ram_bytes = 0;
    double  cache_hit_ratio = 0.0;
    int     peak_connections = 0;

    SPI_connect();
    SPI_exec("SET LOCAL statement_timeout = '500ms'", 0);

    /* Get shared_buffers in bytes */
    ret = SPI_execute(
        "SELECT pg_size_bytes(current_setting('shared_buffers'))", true, 0);
    if (ret == SPI_OK_SELECT && SPI_processed > 0)
        shared_buffers_bytes = sage_spi_getval_int64(0, 0);

    /* Get effective_cache_size in bytes */
    ret = SPI_execute(
        "SELECT pg_size_bytes(current_setting('effective_cache_size'))",
        true, 0);
    if (ret == SPI_OK_SELECT && SPI_processed > 0)
        effective_cache_size_bytes = sage_spi_getval_int64(0, 0);

    /* Get work_mem in bytes */
    ret = SPI_execute(
        "SELECT pg_size_bytes(current_setting('work_mem'))", true, 0);
    if (ret == SPI_OK_SELECT && SPI_processed > 0)
        work_mem_bytes = sage_spi_getval_int64(0, 0);

    /* Get maintenance_work_mem in bytes */
    ret = SPI_execute(
        "SELECT pg_size_bytes(current_setting('maintenance_work_mem'))",
        true, 0);
    if (ret == SPI_OK_SELECT && SPI_processed > 0)
        maintenance_work_mem_bytes = sage_spi_getval_int64(0, 0);

    /* Get max_connections */
    ret = SPI_execute(
        "SELECT current_setting('max_connections')::int", true, 0);
    if (ret == SPI_OK_SELECT && SPI_processed > 0)
        max_connections_val = (int) sage_spi_getval_int64(0, 0);

    /* Get checkpoint_completion_target */
    ret = SPI_execute(
        "SELECT current_setting('checkpoint_completion_target')::float",
        true, 0);
    if (ret == SPI_OK_SELECT && SPI_processed > 0)
        checkpoint_target = sage_spi_getval_float(0, 0);

    /* Get random_page_cost */
    ret = SPI_execute(
        "SELECT current_setting('random_page_cost')::float", true, 0);
    if (ret == SPI_OK_SELECT && SPI_processed > 0)
        random_page_cost_val = sage_spi_getval_float(0, 0);

    /* Estimate total system RAM — skip pg_sysconf (not standard).
     * Go straight to /proc/meminfo fallback below. */
    total_ram_bytes = 0;

    if (total_ram_bytes <= 0)
    {
        /*
         * Fallback: try pg_read_file on /proc/meminfo (Linux only).
         * Use a savepoint so that if the query errors (e.g. not Linux,
         * no permission), we can roll back cleanly without corrupting
         * the enclosing SPI connection state.
         */
        SPI_execute("SAVEPOINT _sage_meminfo", false, 0);
        PG_TRY();
        {
            ret = SPI_execute(
                "SELECT (regexp_match(pg_read_file('/proc/meminfo', 0, 256), "
                "        'MemTotal:\\s+(\\d+)'))[1]::bigint * 1024",
                true, 0);
            if (ret == SPI_OK_SELECT && SPI_processed > 0 &&
                !sage_spi_isnull(0, 0))
                total_ram_bytes = sage_spi_getval_int64(0, 0);
            SPI_execute("RELEASE SAVEPOINT _sage_meminfo", false, 0);
        }
        PG_CATCH();
        {
            FlushErrorState();
            SPI_execute("ROLLBACK TO SAVEPOINT _sage_meminfo", false, 0);
            SPI_execute("RELEASE SAVEPOINT _sage_meminfo", false, 0);
            /* Not on Linux or no permission — use heuristic */
        }
        PG_END_TRY();
    }

    /* If still unknown, estimate as 2x effective_cache_size */
    if (total_ram_bytes <= 0)
        total_ram_bytes = effective_cache_size_bytes * 2;

    /* Cache hit ratio from pg_stat_database */
    ret = SPI_execute(
        "SELECT CASE WHEN (blks_hit + blks_read) > 0 "
        "    THEN 100.0 * blks_hit / (blks_hit + blks_read) "
        "    ELSE 100.0 END "
        "FROM pg_stat_database WHERE datname = current_database()",
        true, 0);
    if (ret == SPI_OK_SELECT && SPI_processed > 0)
        cache_hit_ratio = sage_spi_getval_float(0, 0);

    /* Peak connections (current active) */
    ret = SPI_execute(
        "SELECT count(*) FROM pg_stat_activity "
        "WHERE state != 'idle' AND pid != pg_backend_pid()",
        true, 0);
    if (ret == SPI_OK_SELECT && SPI_processed > 0)
        peak_connections = (int) sage_spi_getval_int64(0, 0);

    /* Now generate findings for misconfigurations */

    /* shared_buffers < 25% of RAM */
    if (total_ram_bytes > 0 &&
        shared_buffers_bytes < (int64)(total_ram_bytes * 0.25))
    {
        StringInfoData detail, rec, rec_sql;
        int64 recommended = (int64)(total_ram_bytes * 0.25);

        initStringInfo(&detail);
        initStringInfo(&rec);
        initStringInfo(&rec_sql);

        appendStringInfo(&detail,
            "{\"param\": \"shared_buffers\", "
            "\"current_bytes\": " INT64_FORMAT ", "
            "\"recommended_bytes\": " INT64_FORMAT ", "
            "\"total_ram_bytes\": " INT64_FORMAT "}",
            shared_buffers_bytes, recommended, total_ram_bytes);

        appendStringInfo(&rec,
            "shared_buffers is set to %s but total RAM is ~%s. "
            "Recommend setting shared_buffers to ~25%% of RAM (%s).",
            psprintf("%.0f MB", (double)shared_buffers_bytes / 1048576.0),
            psprintf("%.0f GB", (double)total_ram_bytes / 1073741824.0),
            psprintf("%.0f MB", (double)recommended / 1048576.0));

        appendStringInfo(&rec_sql,
            "ALTER SYSTEM SET shared_buffers = '%dMB'; -- requires restart",
            (int)(recommended / 1048576));

        sage_upsert_finding("config", "warning",
                            "parameter", "shared_buffers",
                            "shared_buffers below recommended 25% of RAM",
                            detail.data, rec.data, rec_sql.data, NULL);

        pfree(detail.data);
        pfree(rec.data);
        pfree(rec_sql.data);
    }

    /* effective_cache_size < 50% of RAM */
    if (total_ram_bytes > 0 &&
        effective_cache_size_bytes < (int64)(total_ram_bytes * 0.50))
    {
        StringInfoData detail, rec, rec_sql;
        int64 recommended = (int64)(total_ram_bytes * 0.75);

        initStringInfo(&detail);
        initStringInfo(&rec);
        initStringInfo(&rec_sql);

        appendStringInfo(&detail,
            "{\"param\": \"effective_cache_size\", "
            "\"current_bytes\": " INT64_FORMAT ", "
            "\"recommended_bytes\": " INT64_FORMAT ", "
            "\"total_ram_bytes\": " INT64_FORMAT "}",
            effective_cache_size_bytes, recommended, total_ram_bytes);

        appendStringInfo(&rec,
            "effective_cache_size is set to %s but total RAM is ~%s. "
            "Recommend setting to ~75%% of RAM (%s) so the planner "
            "makes better cost estimates.",
            psprintf("%.0f MB", (double)effective_cache_size_bytes / 1048576.0),
            psprintf("%.0f GB", (double)total_ram_bytes / 1073741824.0),
            psprintf("%.0f MB", (double)recommended / 1048576.0));

        appendStringInfo(&rec_sql,
            "ALTER SYSTEM SET effective_cache_size = '%dMB';",
            (int)(recommended / 1048576));

        sage_upsert_finding("config", "warning",
                            "parameter", "effective_cache_size",
                            "effective_cache_size below recommended 50% of RAM",
                            detail.data, rec.data, rec_sql.data, NULL);

        pfree(detail.data);
        pfree(rec.data);
        pfree(rec_sql.data);
    }

    /* work_mem very low (< 4MB) — OLAP workloads especially suffer */
    if (work_mem_bytes < 4194304)
    {
        StringInfoData detail, rec, rec_sql;

        initStringInfo(&detail);
        initStringInfo(&rec);
        initStringInfo(&rec_sql);

        appendStringInfo(&detail,
            "{\"param\": \"work_mem\", "
            "\"current_bytes\": " INT64_FORMAT "}",
            work_mem_bytes);

        appendStringInfo(&rec,
            "work_mem is set to %s. For workloads with complex sorts, "
            "hash joins, or CTEs, consider increasing to at least 4-16 MB. "
            "Note: this is per-operation, so total memory can be "
            "max_connections * work_mem * operations_per_query.",
            psprintf("%.0f KB", (double)work_mem_bytes / 1024.0));

        appendStringInfo(&rec_sql,
            "ALTER SYSTEM SET work_mem = '8MB';");

        sage_upsert_finding("config", "info",
                            "parameter", "work_mem",
                            "work_mem may be too low for analytical workloads",
                            detail.data, rec.data, rec_sql.data, NULL);

        pfree(detail.data);
        pfree(rec.data);
        pfree(rec_sql.data);
    }

    /* max_connections vs actual peak: if max >> 10x peak, might be wasteful */
    if (peak_connections > 0 && max_connections_val > peak_connections * 10)
    {
        StringInfoData detail, rec, rec_sql;

        initStringInfo(&detail);
        initStringInfo(&rec);
        initStringInfo(&rec_sql);

        appendStringInfo(&detail,
            "{\"param\": \"max_connections\", "
            "\"current\": %d, \"peak_active\": %d}",
            max_connections_val, peak_connections);

        appendStringInfo(&rec,
            "max_connections is %d but peak active connections observed "
            "is only %d. High max_connections wastes memory for shared "
            "per-connection state. Consider using a connection pooler "
            "(pgbouncer) and reducing max_connections.",
            max_connections_val, peak_connections);

        appendStringInfo(&rec_sql,
            "ALTER SYSTEM SET max_connections = %d; -- requires restart",
            peak_connections * 3 > 100 ? peak_connections * 3 : 100);

        sage_upsert_finding("config", "info",
                            "parameter", "max_connections",
                            "max_connections significantly exceeds peak usage",
                            detail.data, rec.data, rec_sql.data, NULL);

        pfree(detail.data);
        pfree(rec.data);
        pfree(rec_sql.data);
    }

    /* checkpoint_completion_target < 0.9 */
    if (checkpoint_target > 0.0 && checkpoint_target < 0.9)
    {
        StringInfoData detail, rec, rec_sql;

        initStringInfo(&detail);
        initStringInfo(&rec);
        initStringInfo(&rec_sql);

        appendStringInfo(&detail,
            "{\"param\": \"checkpoint_completion_target\", "
            "\"current\": %.2f, \"recommended\": 0.9}",
            checkpoint_target);

        appendStringInfo(&rec,
            "checkpoint_completion_target is %.2f. Setting it to 0.9 "
            "spreads checkpoint I/O over a longer window, reducing "
            "I/O spikes during checkpoints.",
            checkpoint_target);

        appendStringInfo(&rec_sql,
            "ALTER SYSTEM SET checkpoint_completion_target = 0.9;");

        sage_upsert_finding("config", "warning",
                            "parameter", "checkpoint_completion_target",
                            "checkpoint_completion_target below 0.9",
                            detail.data, rec.data, rec_sql.data, NULL);

        pfree(detail.data);
        pfree(rec.data);
        pfree(rec_sql.data);
    }

    /* random_page_cost = 4 likely means default (HDD).
     * On SSDs it should be ~1.1 */
    if (random_page_cost_val >= 3.9 && random_page_cost_val <= 4.1)
    {
        StringInfoData detail, rec, rec_sql;

        initStringInfo(&detail);
        initStringInfo(&rec);
        initStringInfo(&rec_sql);

        appendStringInfo(&detail,
            "{\"param\": \"random_page_cost\", "
            "\"current\": %.1f, \"recommended\": 1.1}",
            random_page_cost_val);

        appendStringInfo(&rec,
            "random_page_cost is %.1f (the HDD default). If your storage "
            "is SSD, set this to 1.1 to encourage the planner to use "
            "index scans more aggressively.",
            random_page_cost_val);

        appendStringInfo(&rec_sql,
            "ALTER SYSTEM SET random_page_cost = 1.1;");

        sage_upsert_finding("config", "warning",
                            "parameter", "random_page_cost",
                            "random_page_cost at HDD default (4.0) — consider SSD tuning",
                            detail.data, rec.data, rec_sql.data, NULL);

        pfree(detail.data);
        pfree(rec.data);
        pfree(rec_sql.data);
    }

    /* Cache hit ratio < 99% */
    if (cache_hit_ratio > 0.0 && cache_hit_ratio < 99.0)
    {
        StringInfoData detail, rec;
        const char *severity;

        initStringInfo(&detail);
        initStringInfo(&rec);

        appendStringInfo(&detail,
            "{\"metric\": \"cache_hit_ratio\", "
            "\"value\": %.2f, \"threshold\": 99.0}",
            cache_hit_ratio);

        if (cache_hit_ratio < 90.0)
            severity = "critical";
        else if (cache_hit_ratio < 95.0)
            severity = "warning";
        else
            severity = "info";

        appendStringInfo(&rec,
            "Database cache hit ratio is %.2f%% (target: >99%%). "
            "This means %.2f%% of block reads go to disk. "
            "Consider increasing shared_buffers or adding more RAM.",
            cache_hit_ratio, 100.0 - cache_hit_ratio);

        sage_upsert_finding("config", severity,
                            "metric", "cache_hit_ratio",
                            psprintf("Cache hit ratio %.1f%% (below 99%%)",
                                     cache_hit_ratio),
                            detail.data, rec.data, NULL, NULL);

        pfree(detail.data);
        pfree(rec.data);
    }

    (void) maintenance_work_mem_bytes;  /* Reserved for future checks */

    SPI_finish();
}

/* ----------------------------------------------------------------
 * sage_analyze_index_bloat
 * ---------------------------------------------------------------- */
void
sage_analyze_index_bloat(void)
{
    int     ret;
    int     i;

    SPI_connect();
    SPI_exec("SET LOCAL statement_timeout = '500ms'", 0);

    ret = SPI_execute(
        "SELECT "
        "    i.schemaname, i.relname as tablename, "
        "    i.indexrelname as indexname, "
        "    pg_relation_size(i.indexrelid) as actual_bytes, "
        "    (SELECT (c.relpages * current_setting('block_size')::int) "
        "     FROM pg_class c WHERE c.oid = i.indexrelid) as estimated_bytes, "
        "    CASE WHEN pg_relation_size(i.indexrelid) > 0 "
        "         THEN 100.0 * (pg_relation_size(i.indexrelid) - "
        "              (SELECT c2.reltuples * 40 "
        "               FROM pg_class c2 WHERE c2.oid = i.indexrelid)) "
        "              / pg_relation_size(i.indexrelid) "
        "         ELSE 0 END as estimated_bloat_pct "
        "FROM pg_stat_user_indexes i "
        "WHERE pg_relation_size(i.indexrelid) > 1048576 "
        "ORDER BY pg_relation_size(i.indexrelid) DESC "
        "LIMIT 50",
        true, 0);

    if (ret == SPI_OK_SELECT && SPI_processed > 0)
    {
        for (i = 0; i < (int) SPI_processed; i++)
        {
            char   *schemaname  = pstrdup(sage_spi_getval_str(i, 0));
            char   *tablename   = pstrdup(sage_spi_getval_str(i, 1));
            char   *indexname   = pstrdup(sage_spi_getval_str(i, 2));
            int64   actual_bytes = sage_spi_getval_int64(i, 3);
            int64   estimated_bytes = sage_spi_getval_int64(i, 4);
            double  bloat_pct   = sage_spi_getval_float(i, 5);

            if (bloat_pct < (double) sage_index_bloat_threshold)
                continue;

            {
                StringInfoData detail, object_id, title_buf, rec, rec_sql;
                const char *severity;

                initStringInfo(&detail);
                initStringInfo(&object_id);
                initStringInfo(&title_buf);
                initStringInfo(&rec);
                initStringInfo(&rec_sql);

                appendStringInfo(&object_id, "%s.%s", schemaname, indexname);

                appendStringInfo(&title_buf,
                    "Index %s.%s bloated by ~%.0f%% (%s actual)",
                    schemaname, indexname, bloat_pct,
                    actual_bytes > 1073741824
                        ? psprintf("%.1f GB", (double)actual_bytes / 1073741824.0)
                        : psprintf("%.1f MB", (double)actual_bytes / 1048576.0));

                {
                    char *detail_json = sage_format_jsonb_object(
                        "index", psprintf("%s.%s", schemaname, indexname),
                        "table", psprintf("%s.%s", schemaname, tablename),
                        "actual_bytes", psprintf(INT64_FORMAT, actual_bytes),
                        "estimated_bytes", psprintf(INT64_FORMAT, estimated_bytes),
                        "estimated_bloat_pct", psprintf("%.1f", bloat_pct),
                        NULL);
                    appendStringInfoString(&detail, detail_json);
                    pfree(detail_json);
                }

                if (bloat_pct >= 80.0)
                    severity = "critical";
                else if (bloat_pct >= 50.0)
                    severity = "warning";
                else
                    severity = "info";

                appendStringInfo(&rec,
                    "Index %s.%s on %s.%s has an estimated bloat of "
                    "%.0f%%. The actual size is %s vs ~%s estimated "
                    "useful data. Rebuilding the index with REINDEX "
                    "CONCURRENTLY will reclaim space.",
                    schemaname, indexname, schemaname, tablename,
                    bloat_pct,
                    psprintf("%.1f MB", (double)actual_bytes / 1048576.0),
                    psprintf("%.1f MB", (double)estimated_bytes / 1048576.0));

                appendStringInfo(&rec_sql,
                    "REINDEX INDEX CONCURRENTLY %s.%s;",
                    schemaname, indexname);

                sage_upsert_finding("index_bloat", severity,
                                    "index", object_id.data,
                                    title_buf.data, detail.data,
                                    rec.data, rec_sql.data, NULL);

                pfree(detail.data);
                pfree(object_id.data);
                pfree(title_buf.data);
                pfree(rec.data);
                pfree(rec_sql.data);
            }
        }
    }

    SPI_finish();
}

/* ----------------------------------------------------------------
 * sage_analyze_index_write_penalty
 * ---------------------------------------------------------------- */
void
sage_analyze_index_write_penalty(void)
{
    int     ret;
    int     i;

    SPI_connect();
    SPI_exec("SET LOCAL statement_timeout = '500ms'", 0);

    ret = SPI_execute(
        "SELECT i.schemaname, i.relname, i.indexrelname, "
        "       i.idx_scan, "
        "       t.n_tup_ins + t.n_tup_upd + t.n_tup_del as mutations, "
        "       pg_relation_size(i.indexrelid) as index_bytes, "
        "       CASE WHEN i.idx_scan > 0 "
        "            THEN (t.n_tup_ins + t.n_tup_upd + t.n_tup_del)::float "
        "                 / i.idx_scan "
        "            ELSE -1 END as write_read_ratio "
        "FROM pg_stat_user_indexes i "
        "JOIN pg_stat_user_tables t ON t.relid = i.relid "
        "WHERE t.n_tup_ins + t.n_tup_upd + t.n_tup_del > 1000 "
        "ORDER BY write_read_ratio DESC "
        "LIMIT 20",
        true, 0);

    if (ret == SPI_OK_SELECT && SPI_processed > 0)
    {
        for (i = 0; i < (int) SPI_processed; i++)
        {
            char   *schemaname   = pstrdup(sage_spi_getval_str(i, 0));
            char   *relname      = pstrdup(sage_spi_getval_str(i, 1));
            char   *indexrelname = pstrdup(sage_spi_getval_str(i, 2));
            int64   idx_scan     = sage_spi_getval_int64(i, 3);
            int64   mutations    = sage_spi_getval_int64(i, 4);
            int64   index_bytes  = sage_spi_getval_int64(i, 5);
            double  wr_ratio     = sage_spi_getval_float(i, 6);

            /* Only flag indexes with very high write-to-read ratio:
             * write_read_ratio > 100 and idx_scan < 10 */
            if (wr_ratio <= 100.0 || idx_scan >= 10)
                continue;

            /* Also flag the case where idx_scan == 0
             * (wr_ratio will be -1), which means the index is
             * never read but constantly written to. */
            if (wr_ratio < 0 && idx_scan > 0)
                continue;

            {
                StringInfoData detail, object_id, title_buf, rec, drop_sql;

                initStringInfo(&detail);
                initStringInfo(&object_id);
                initStringInfo(&title_buf);
                initStringInfo(&rec);
                initStringInfo(&drop_sql);

                appendStringInfo(&object_id, "%s.%s",
                                 schemaname, indexrelname);

                appendStringInfo(&title_buf,
                    "High write penalty index %s.%s "
                    "(" INT64_FORMAT " mutations vs " INT64_FORMAT " scans)",
                    schemaname, indexrelname, mutations, idx_scan);

                {
                    char *detail_json = sage_format_jsonb_object(
                        "index", psprintf("%s.%s", schemaname, indexrelname),
                        "table", psprintf("%s.%s", schemaname, relname),
                        "idx_scan", psprintf(INT64_FORMAT, idx_scan),
                        "mutations", psprintf(INT64_FORMAT, mutations),
                        "index_bytes", psprintf(INT64_FORMAT, index_bytes),
                        "write_read_ratio", psprintf("%.1f", wr_ratio < 0 ? (double)mutations : wr_ratio),
                        NULL);
                    appendStringInfoString(&detail, detail_json);
                    pfree(detail_json);
                }

                appendStringInfo(&rec,
                    "Index %s.%s on %s.%s has %s and "
                    INT64_FORMAT " mutations (inserts+updates+deletes) but "
                    "only " INT64_FORMAT " scans. This index is consuming "
                    "significant write I/O with minimal read benefit. "
                    "Consider dropping it.",
                    schemaname, indexrelname, schemaname, relname,
                    index_bytes > 1073741824
                        ? psprintf("%.1f GB", (double)index_bytes / 1073741824.0)
                        : psprintf("%.1f MB", (double)index_bytes / 1048576.0),
                    mutations, idx_scan);

                appendStringInfo(&drop_sql,
                    "DROP INDEX CONCURRENTLY %s.%s;",
                    schemaname, indexrelname);

                sage_upsert_finding("index_write_penalty",
                    idx_scan == 0 ? "critical" : "warning",
                    "index", object_id.data,
                    title_buf.data, detail.data,
                    rec.data, drop_sql.data,
                    psprintf("-- Retrieve original DDL: "
                             "SELECT indexdef FROM pg_indexes "
                             "WHERE schemaname = '%s' AND indexname = '%s';",
                             schemaname, indexrelname));

                pfree(detail.data);
                pfree(object_id.data);
                pfree(title_buf.data);
                pfree(rec.data);
                pfree(drop_sql.data);
            }
        }
    }

    SPI_finish();
}

/* ----------------------------------------------------------------
 * sage_analyze_ha_state
 * ---------------------------------------------------------------- */
void
sage_analyze_ha_state(void)
{
    int     ret;
    bool    in_recovery = false;

    SPI_connect();
    SPI_exec("SET LOCAL statement_timeout = '500ms'", 0);

    ret = SPI_execute("SELECT pg_is_in_recovery()", true, 0);
    if (ret == SPI_OK_SELECT && SPI_processed > 0)
    {
        char *val = pstrdup(sage_spi_getval_str(0, 0));

        in_recovery = (val && (val[0] == 't' || val[0] == 'T'));
        pfree(val);
    }

    /* Update shared state */
    if (sage_state)
    {
        bool previous;

        LWLockAcquire(sage_state->lock, LW_EXCLUSIVE);
        previous = sage_state->is_in_recovery;
        sage_state->is_in_recovery = in_recovery;

        /* Track role flips */
        if (previous != in_recovery)
        {
            sage_state->role_flip_count++;
            ereport(LOG,
                    (errmsg("pg_sage analyzer: HA role change detected "
                            "(in_recovery: %s -> %s, flip count: %d)",
                            previous ? "true" : "false",
                            in_recovery ? "true" : "false",
                            sage_state->role_flip_count)));
        }

        LWLockRelease(sage_state->lock);
    }

    /* If in recovery, generate an informational finding */
    if (in_recovery)
    {
        sage_upsert_finding("ha_state", "info",
                            "cluster", "recovery_mode",
                            "Database is in recovery mode (standby)",
                            "{\"in_recovery\": true}",
                            "This server is a standby replica. "
                            "Tier 3 autonomous actions are suppressed. "
                            "Read-only analysis continues.",
                            NULL, NULL);
    }
    else
    {
        /* Resolve the recovery finding if we previously had one */
        sage_resolve_finding("ha_state", "recovery_mode");
    }

    /* Check replication lag from latest replication snapshot */
    if (!in_recovery)
    {
        ret = SPI_execute(
            "SELECT (elem->>'replay_lag') as replay_lag, "
            "       (elem->>'client_addr') as client_addr, "
            "       (elem->>'application_name') as app_name "
            "FROM sage.snapshots s, "
            "     jsonb_array_elements(s.data) elem "
            "WHERE s.category = 'replication' "
            "  AND s.collected_at > now() - interval '10 minutes' "
            "ORDER BY s.collected_at DESC "
            "LIMIT 5",
            true, 0);

        if (ret == SPI_OK_SELECT && SPI_processed > 0)
        {
            int j;

            for (j = 0; j < (int) SPI_processed; j++)
            {
                char *replay_lag  = pstrdup(sage_spi_getval_str(j, 0));
                char *client_addr = sage_spi_isnull(j, 1) ? pstrdup("unknown")
                                        : pstrdup(sage_spi_getval_str(j, 1));
                char *app_name    = sage_spi_isnull(j, 2) ? pstrdup("unknown")
                                        : pstrdup(sage_spi_getval_str(j, 2));

                if (replay_lag && strlen(replay_lag) > 0)
                {
                    StringInfoData detail, object_id;

                    initStringInfo(&detail);
                    initStringInfo(&object_id);

                    appendStringInfo(&object_id, "replication:%s", client_addr);

                    {
                        char *detail_json = sage_format_jsonb_object(
                            "client_addr", client_addr,
                            "application_name", app_name,
                            "replay_lag", replay_lag,
                            NULL);
                        appendStringInfoString(&detail, detail_json);
                        pfree(detail_json);
                    }

                    sage_upsert_finding("ha_state", "info",
                                        "replication", object_id.data,
                                        psprintf("Replication lag for %s: %s",
                                                 app_name, replay_lag),
                                        detail.data,
                                        "Monitor replication lag. Significant "
                                        "lag may indicate network issues or "
                                        "standby performance problems.",
                                        NULL, NULL);

                    pfree(detail.data);
                    pfree(object_id.data);
                }

                pfree(replay_lag);
                pfree(client_addr);
                pfree(app_name);
            }
        }
    }

    SPI_finish();
}

/* ----------------------------------------------------------------
 * sage_run_retention_cleanup
 *
 * Delete old data in batches using ctid-based subqueries
 * (PostgreSQL doesn't support LIMIT on DELETE directly).
 * ---------------------------------------------------------------- */
void
sage_run_retention_cleanup(void)
{
    StringInfoData sql;

    SPI_connect();
    SPI_exec("SET LOCAL statement_timeout = '5000ms'", 0);

    /* Clean old snapshots */
    initStringInfo(&sql);
    appendStringInfo(&sql,
        "DELETE FROM sage.snapshots WHERE ctid IN ("
        "  SELECT ctid FROM sage.snapshots "
        "  WHERE collected_at < now() - interval '%d days' "
        "  LIMIT 1000"
        ")",
        sage_retention_snapshots);
    SPI_execute(sql.data, false, 0);
    resetStringInfo(&sql);

    /* Clean resolved findings */
    appendStringInfo(&sql,
        "DELETE FROM sage.findings WHERE ctid IN ("
        "  SELECT ctid FROM sage.findings "
        "  WHERE status = 'resolved' "
        "    AND resolved_at < now() - interval '%d days' "
        "  LIMIT 1000"
        ")",
        sage_retention_findings);
    SPI_execute(sql.data, false, 0);
    resetStringInfo(&sql);

    /* Clean old explain cache */
    appendStringInfo(&sql,
        "DELETE FROM sage.explain_cache WHERE ctid IN ("
        "  SELECT ctid FROM sage.explain_cache "
        "  WHERE captured_at < now() - interval '%d days' "
        "  LIMIT 1000"
        ")",
        sage_retention_explains);
    SPI_execute(sql.data, false, 0);
    resetStringInfo(&sql);

    /* Clean old action log */
    appendStringInfo(&sql,
        "DELETE FROM sage.action_log WHERE ctid IN ("
        "  SELECT ctid FROM sage.action_log "
        "  WHERE executed_at < now() - interval '%d days' "
        "  LIMIT 1000"
        ")",
        sage_retention_actions);
    SPI_execute(sql.data, false, 0);

    pfree(sql.data);

    SPI_finish();
}

/* ================================================================
 * Background worker entry point
 * ================================================================ */

PGDLLEXPORT void
sage_analyzer_main(Datum main_arg)
{
    char       *dbname;

    /* Set up signal handlers */
    pqsignal(SIGHUP, sage_analyzer_sighup);
    pqsignal(SIGTERM, sage_analyzer_sigterm);

    /* Allow signals */
    BackgroundWorkerUnblockSignals();

    /* Determine database name from bgw_extra */
    dbname = MyBgworkerEntry->bgw_extra;
    if (!dbname || dbname[0] == '\0')
        dbname = "postgres";

    /* Connect to the database */
    BackgroundWorkerInitializeConnection(dbname, NULL, 0);

    ereport(LOG,
            (errmsg("pg_sage analyzer: started, connected to \"%s\", "
                    "interval %d s",
                    dbname, sage_analyzer_interval)));

    /* Mark as running */
    if (sage_state)
    {
        LWLockAcquire(sage_state->lock, LW_EXCLUSIVE);
        sage_state->analyzer_running = true;
        LWLockRelease(sage_state->lock);
    }

    /* Main loop */
    while (!got_sigterm)
    {
        instr_time      cycle_start, cycle_end;
        double          cycle_ms;

        /* Compute adaptive interval based on workload, then wait */
        {
            int wait_ms;

            StartTransactionCommand();
            PushActiveSnapshot(GetTransactionSnapshot());
            wait_ms = compute_adaptive_interval();
            PopActiveSnapshot();
            CommitTransactionCommand();

            (void) WaitLatch(MyLatch,
                           WL_LATCH_SET | WL_TIMEOUT | WL_EXIT_ON_PM_DEATH,
                           wait_ms,
                           PG_WAIT_EXTENSION);
        }

        ResetLatch(MyLatch);

        /* Handle SIGHUP: reload config */
        if (got_sighup)
        {
            got_sighup = false;
            ProcessConfigFile(PGC_SIGHUP);
        }

        /* Check termination */
        if (got_sigterm)
            break;

        /* Skip if disabled */
        if (!sage_enabled)
            continue;

        /* Check emergency stop */
        if (sage_state && sage_state->emergency_stopped)
            continue;

        /* Check circuit breaker */
        if (!sage_circuit_check())
        {
            sage_circuit_record_skip();
            continue;
        }

        /* Check recovery mode — still run analysis but note it */
        (void) sage_check_recovery_mode();

        INSTR_TIME_SET_CURRENT(cycle_start);

        /* Start a transaction for the analysis cycle */
        StartTransactionCommand();
        SPI_connect();
        PushActiveSnapshot(GetTransactionSnapshot());

        /* Auto-unsuppress expired findings at the start of each cycle */
        sage_check_suppressions();

        SPI_finish();
        PopActiveSnapshot();
        CommitTransactionCommand();

        /* Run each analysis function in its own transaction,
         * wrapped in PG_TRY/PG_CATCH via run_analysis(). */

        /* Tier 1: Index analysis */
        StartTransactionCommand();
        PushActiveSnapshot(GetTransactionSnapshot());
        run_analysis("unused_indexes", sage_analyze_unused_indexes);
        PopActiveSnapshot();
        CommitTransactionCommand();
        CHECK_FOR_INTERRUPTS();

        StartTransactionCommand();
        PushActiveSnapshot(GetTransactionSnapshot());
        run_analysis("duplicate_indexes", sage_analyze_duplicate_indexes);
        PopActiveSnapshot();
        CommitTransactionCommand();
        CHECK_FOR_INTERRUPTS();

        StartTransactionCommand();
        PushActiveSnapshot(GetTransactionSnapshot());
        run_analysis("index_bloat", sage_analyze_index_bloat);
        PopActiveSnapshot();
        CommitTransactionCommand();
        CHECK_FOR_INTERRUPTS();

        StartTransactionCommand();
        PushActiveSnapshot(GetTransactionSnapshot());
        run_analysis("index_write_penalty", sage_analyze_index_write_penalty);
        PopActiveSnapshot();
        CommitTransactionCommand();
        CHECK_FOR_INTERRUPTS();

        /* Tier 2: Query analysis */
        StartTransactionCommand();
        PushActiveSnapshot(GetTransactionSnapshot());
        run_analysis("slow_queries", sage_analyze_slow_queries);
        PopActiveSnapshot();
        CommitTransactionCommand();
        CHECK_FOR_INTERRUPTS();

        StartTransactionCommand();
        PushActiveSnapshot(GetTransactionSnapshot());
        run_analysis("query_regressions", sage_analyze_query_regressions);
        PopActiveSnapshot();
        CommitTransactionCommand();
        CHECK_FOR_INTERRUPTS();

        StartTransactionCommand();
        PushActiveSnapshot(GetTransactionSnapshot());
        run_analysis("missing_indexes", sage_analyze_missing_indexes);
        PopActiveSnapshot();
        CommitTransactionCommand();
        CHECK_FOR_INTERRUPTS();

        StartTransactionCommand();
        PushActiveSnapshot(GetTransactionSnapshot());
        run_analysis("seq_scans", sage_analyze_seq_scans);
        PopActiveSnapshot();
        CommitTransactionCommand();
        CHECK_FOR_INTERRUPTS();

        /* Tier 3: System-level checks */
        StartTransactionCommand();
        PushActiveSnapshot(GetTransactionSnapshot());
        run_analysis("sequence_exhaustion", sage_analyze_sequence_exhaustion);
        PopActiveSnapshot();
        CommitTransactionCommand();
        CHECK_FOR_INTERRUPTS();

        StartTransactionCommand();
        PushActiveSnapshot(GetTransactionSnapshot());
        run_analysis("config", sage_analyze_config);
        PopActiveSnapshot();
        CommitTransactionCommand();
        CHECK_FOR_INTERRUPTS();

        StartTransactionCommand();
        PushActiveSnapshot(GetTransactionSnapshot());
        run_analysis("ha_state", sage_analyze_ha_state);
        PopActiveSnapshot();
        CommitTransactionCommand();
        CHECK_FOR_INTERRUPTS();

        /* Tier 1.2: Vacuum & bloat */
        StartTransactionCommand();
        PushActiveSnapshot(GetTransactionSnapshot());
        run_analysis("vacuum_bloat", sage_analyze_vacuum_bloat);
        PopActiveSnapshot();
        CommitTransactionCommand();
        CHECK_FOR_INTERRUPTS();

        /* Tier 1.8: Security audit */
        StartTransactionCommand();
        PushActiveSnapshot(GetTransactionSnapshot());
        run_analysis("security", sage_analyze_security);
        PopActiveSnapshot();
        CommitTransactionCommand();
        CHECK_FOR_INTERRUPTS();

        /* Tier 1.9: Replication health */
        StartTransactionCommand();
        PushActiveSnapshot(GetTransactionSnapshot());
        run_analysis("replication_health", sage_analyze_replication_health);
        PopActiveSnapshot();
        CommitTransactionCommand();
        CHECK_FOR_INTERRUPTS();

        /* Tier 2.5: Cost attribution */
        StartTransactionCommand();
        PushActiveSnapshot(GetTransactionSnapshot());
        run_analysis("cost_attribution", sage_analyze_cost_attribution);
        PopActiveSnapshot();
        CommitTransactionCommand();
        CHECK_FOR_INTERRUPTS();

        /* Tier 2.6: Migration review */
        StartTransactionCommand();
        PushActiveSnapshot(GetTransactionSnapshot());
        run_analysis("migration_review", sage_analyze_migration_review);
        PopActiveSnapshot();
        CommitTransactionCommand();
        CHECK_FOR_INTERRUPTS();

        /* Tier 2.7: Schema design review */
        StartTransactionCommand();
        PushActiveSnapshot(GetTransactionSnapshot());
        run_analysis("schema_design", sage_analyze_schema_design);
        PopActiveSnapshot();
        CommitTransactionCommand();
        CHECK_FOR_INTERRUPTS();

        /* Retention cleanup */
        StartTransactionCommand();
        PushActiveSnapshot(GetTransactionSnapshot());
        run_analysis("retention_cleanup", sage_run_retention_cleanup);
        PopActiveSnapshot();
        CommitTransactionCommand();
        CHECK_FOR_INTERRUPTS();

        /* Tier 3: Action executor */
        StartTransactionCommand();
        PushActiveSnapshot(GetTransactionSnapshot());
        run_analysis("action_executor", sage_action_executor_run);
        PopActiveSnapshot();
        CommitTransactionCommand();
        CHECK_FOR_INTERRUPTS();

        /* Tier 3: Rollback check */
        StartTransactionCommand();
        PushActiveSnapshot(GetTransactionSnapshot());
        run_analysis("rollback_check", sage_rollback_check);
        PopActiveSnapshot();
        CommitTransactionCommand();

        INSTR_TIME_SET_CURRENT(cycle_end);
        INSTR_TIME_SUBTRACT(cycle_end, cycle_start);
        cycle_ms = INSTR_TIME_GET_MILLISEC(cycle_end);

        /* Record timing in shared state */
        if (sage_state)
        {
            LWLockAcquire(sage_state->lock, LW_EXCLUSIVE);
            sage_state->last_analyze_time = GetCurrentTimestamp();
            sage_state->last_analyzer_duration_ms = cycle_ms;
            LWLockRelease(sage_state->lock);
        }

        sage_circuit_record_success();

        ereport(DEBUG1,
                (errmsg("pg_sage analyzer: cycle completed in %.1f ms",
                        cycle_ms)));
    }

    /* Mark as stopped */
    if (sage_state)
    {
        LWLockAcquire(sage_state->lock, LW_EXCLUSIVE);
        sage_state->analyzer_running = false;
        LWLockRelease(sage_state->lock);
    }

    ereport(LOG,
            (errmsg("pg_sage analyzer: shutting down")));

    proc_exit(0);
}
