/*
 * briefing.c — Daily briefing worker and generation
 *
 * Background worker that produces daily health briefings,
 * plus on-demand sage_briefing() and sage_diagnose() SQL functions.
 * AGPL-3.0 License
 */

#include "pg_sage.h"

#include <string.h>
#include <curl/curl.h>
#include "lib/stringinfo.h"
#include "executor/spi.h"
#include "utils/builtins.h"
#include "tcop/utility.h"
#include "postmaster/bgworker.h"
#include "storage/latch.h"
#include "access/xact.h"
#include "utils/resowner.h"
#include "utils/snapmgr.h"

/* Forward declarations */
static void briefing_sigterm(SIGNAL_ARGS);
static void briefing_sighup(SIGNAL_ARGS);
static bool is_briefing_due(void);
static char *spi_getval_alloc(int row, int col);
static char *spi_query_simple(const char *sql);
static size_t slack_write_callback(void *contents, size_t size, size_t nmemb,
                                   void *userp);

/* Volatile flags for signal handling */
static volatile sig_atomic_t got_sigterm = false;
static volatile sig_atomic_t got_sighup = false;

/* ----------------------------------------------------------------
 * Signal handlers
 * ---------------------------------------------------------------- */
static void
briefing_sigterm(SIGNAL_ARGS)
{
    int save_errno = errno;

    got_sigterm = true;
    SetLatch(MyLatch);

    errno = save_errno;
}

static void
briefing_sighup(SIGNAL_ARGS)
{
    int save_errno = errno;

    got_sighup = true;
    SetLatch(MyLatch);

    errno = save_errno;
}

/* ----------------------------------------------------------------
 * curl write callback for Slack delivery
 * ---------------------------------------------------------------- */
static size_t
slack_write_callback(void *contents, size_t size, size_t nmemb, void *userp)
{
    /* Discard response body */
    return size * nmemb;
}

/* ----------------------------------------------------------------
 * SPI helper: query and return all results as a single string
 * ---------------------------------------------------------------- */
static char *
spi_query_simple(const char *sql)
{
    StringInfoData  result;
    int             ret;
    int             i, j;

    initStringInfo(&result);

    ret = SPI_execute(sql, true, 0);
    if (ret != SPI_OK_SELECT || SPI_tuptable == NULL)
        return result.data;

    for (i = 0; i < (int) SPI_processed; i++)
    {
        for (j = 0; j < SPI_tuptable->tupdesc->natts; j++)
        {
            char *str = SPI_getvalue(SPI_tuptable->vals[i],
                                     SPI_tuptable->tupdesc,
                                     j + 1);
            if (str != NULL)
                appendStringInfoString(&result, str);
            else
                appendStringInfoString(&result, "NULL");

            if (j < SPI_tuptable->tupdesc->natts - 1)
                appendStringInfoString(&result, " | ");
        }
        appendStringInfoChar(&result, '\n');
    }

    return result.data;
}

/* ----------------------------------------------------------------
 * SPI helper: get a palloc'd copy of a single value
 * ---------------------------------------------------------------- */
static char *
spi_getval_alloc(int row, int col)
{
    char *val;

    if (SPI_tuptable == NULL || row >= (int) SPI_processed)
        return pstrdup("");

    val = SPI_getvalue(SPI_tuptable->vals[row], SPI_tuptable->tupdesc, col);
    return val ? pstrdup(val) : pstrdup("NULL");
}

/* ----------------------------------------------------------------
 * is_briefing_due
 *
 * Simple schedule check: parse sage_briefing_schedule for an hour
 * value and compare to current hour.  Supports formats like "08:00"
 * or just "8".  Also checks that we haven't already briefed today.
 * ---------------------------------------------------------------- */
static bool
is_briefing_due(void)
{
    int             target_hour = 8;    /* default: 08:00 */
    struct pg_tm    tm;
    fsec_t          fsec;
    int             tz;
    TimestampTz     now_ts;

    /* Parse schedule hour */
    if (sage_briefing_schedule != NULL && sage_briefing_schedule[0] != '\0')
    {
        char *colon = strchr(sage_briefing_schedule, ':');

        if (colon != NULL)
        {
            /* "HH:MM" format */
            char buf[8];
            int  len = colon - sage_briefing_schedule;

            if (len > 0 && len < (int) sizeof(buf))
            {
                memcpy(buf, sage_briefing_schedule, len);
                buf[len] = '\0';
                target_hour = atoi(buf);
            }
        }
        else
        {
            target_hour = atoi(sage_briefing_schedule);
        }

        if (target_hour < 0 || target_hour > 23)
            target_hour = 8;
    }

    /* Get current time */
    now_ts = GetCurrentTimestamp();
    if (timestamp2tm(now_ts, &tz, &tm, &fsec, NULL, NULL) != 0)
        return false;

    /* Must be the right hour */
    if (tm.tm_hour != target_hour)
        return false;

    /* Check we haven't already run a briefing today */
    if (sage_state != NULL)
    {
        struct pg_tm last_tm;
        fsec_t       last_fsec;
        int          last_tz;

        LWLockAcquire(sage_state->lock, LW_SHARED);

        if (sage_state->last_briefing_time != 0)
        {
            if (timestamp2tm(sage_state->last_briefing_time, &last_tz,
                             &last_tm, &last_fsec, NULL, NULL) == 0)
            {
                if (last_tm.tm_year == tm.tm_year &&
                    last_tm.tm_mon  == tm.tm_mon  &&
                    last_tm.tm_mday == tm.tm_mday)
                {
                    LWLockRelease(sage_state->lock);
                    return false;
                }
            }
        }

        LWLockRelease(sage_state->lock);
    }

    return true;
}

/* ----------------------------------------------------------------
 * sage_briefing_main — background worker entry point
 * ---------------------------------------------------------------- */
PGDLLEXPORT void
sage_briefing_main(Datum main_arg)
{
    char *dbname;

    /* Register signal handlers */
    pqsignal(SIGTERM, briefing_sigterm);
    pqsignal(SIGHUP, briefing_sighup);

    BackgroundWorkerUnblockSignals();

    /* Determine database name from bgw_extra */
    dbname = MyBgworkerEntry->bgw_extra;
    if (!dbname || dbname[0] == '\0')
        dbname = "postgres";

    /* Connect to the database */
    BackgroundWorkerInitializeConnection(dbname, NULL, 0);

    elog(LOG, "pg_sage: briefing worker started");

    /* Mark worker as running */
    if (sage_state != NULL)
    {
        LWLockAcquire(sage_state->lock, LW_EXCLUSIVE);
        sage_state->briefing_running = true;
        LWLockRelease(sage_state->lock);
    }

    /* Main loop */
    while (!got_sigterm)
    {
        int rc;

        /* Wait for 60 seconds or until signalled */
        rc = WaitLatch(MyLatch,
                       WL_LATCH_SET | WL_TIMEOUT | WL_POSTMASTER_DEATH,
                       60000L,
                       PG_WAIT_EXTENSION);

        ResetLatch(MyLatch);

        if (rc & WL_POSTMASTER_DEATH)
            proc_exit(1);

        if (got_sigterm)
            break;

        if (got_sighup)
        {
            got_sighup = false;
            ProcessConfigFile(PGC_SIGHUP);
        }

        /* Check if extension is enabled and not emergency-stopped */
        if (!sage_enabled)
            continue;

        if (sage_state != NULL && sage_state->emergency_stopped)
            continue;

        /* Check if it's time for a briefing */
        if (!is_briefing_due())
            continue;

        /* Generate and deliver the briefing */
        PG_TRY();
        {
            char   *content;
            bool    use_llm;

            elog(LOG, "pg_sage: generating daily briefing");

            SetCurrentStatementStartTimestamp();
            StartTransactionCommand();
            PushActiveSnapshot(GetTransactionSnapshot());

            use_llm = sage_llm_available();
            content = sage_generate_briefing(use_llm);

            if (content != NULL && content[0] != '\0')
                sage_deliver_briefing(content, NULL);

            PopActiveSnapshot();
            CommitTransactionCommand();

            /* Record briefing time */
            if (sage_state != NULL)
            {
                LWLockAcquire(sage_state->lock, LW_EXCLUSIVE);
                sage_state->last_briefing_time = GetCurrentTimestamp();
                LWLockRelease(sage_state->lock);
            }

            elog(LOG, "pg_sage: daily briefing complete");
        }
        PG_CATCH();
        {
            EmitErrorReport();
            FlushErrorState();

            if (ActiveSnapshotSet())
                PopActiveSnapshot();
            AbortCurrentTransaction();

            elog(WARNING, "pg_sage: briefing generation failed");
        }
        PG_END_TRY();
    }

    /* Mark worker as stopped */
    if (sage_state != NULL)
    {
        LWLockAcquire(sage_state->lock, LW_EXCLUSIVE);
        sage_state->briefing_running = false;
        LWLockRelease(sage_state->lock);
    }

    elog(LOG, "pg_sage: briefing worker shutting down");
    proc_exit(0);
}

/* ----------------------------------------------------------------
 * sage_generate_briefing
 *
 * Build a daily health briefing, optionally using the LLM.
 * Returns palloc'd string.
 * ---------------------------------------------------------------- */
char *
sage_generate_briefing(bool use_llm)
{
    StringInfoData  briefing;
    StringInfoData  data_buf;
    char           *section;
    char           *content = NULL;
    int             connected;
    int             critical_count = 0;
    int             warning_count = 0;
    int             info_count = 0;
    char           *new_findings = NULL;
    char           *resolved = NULL;
    char           *actions = NULL;
    char           *system_health = NULL;
    char           *sage_health = NULL;
    TimestampTz     now_ts;
    char            ts_buf[128];

    initStringInfo(&briefing);
    initStringInfo(&data_buf);

    connected = SPI_connect();
    if (connected != SPI_OK_CONNECT)
    {
        elog(WARNING, "pg_sage: SPI_connect failed in briefing generation");
        return pstrdup("[Error: unable to query database for briefing]");
    }

    PG_TRY();
    {
        int ret;
        int i;

        /* 1. Open findings summary */
        ret = SPI_execute(
            "SELECT severity, count(*) FROM sage.findings "
            "WHERE status = 'open' GROUP BY severity", true, 0);

        if (ret == SPI_OK_SELECT && SPI_tuptable != NULL)
        {
            for (i = 0; i < (int) SPI_processed; i++)
            {
                char *sev = SPI_getvalue(SPI_tuptable->vals[i],
                                         SPI_tuptable->tupdesc, 1);
                char *cnt = SPI_getvalue(SPI_tuptable->vals[i],
                                         SPI_tuptable->tupdesc, 2);
                int count_val = cnt ? atoi(cnt) : 0;

                if (sev != NULL)
                {
                    if (strcmp(sev, "critical") == 0)
                        critical_count = count_val;
                    else if (strcmp(sev, "warning") == 0)
                        warning_count = count_val;
                    else if (strcmp(sev, "info") == 0)
                        info_count = count_val;
                }
            }
        }

        /* 2. New findings since last briefing */
        new_findings = spi_query_simple(
            "SELECT category, severity, title, object_identifier, recommendation "
            "FROM sage.findings "
            "WHERE created_at > (SELECT COALESCE(max(period_end), now() - interval '1 day') "
            "                    FROM sage.briefings) "
            "ORDER BY severity DESC, created_at DESC");

        /* 3. Resolved findings */
        resolved = spi_query_simple(
            "SELECT category, title, object_identifier, resolved_at "
            "FROM sage.findings "
            "WHERE status = 'resolved' "
            "  AND resolved_at > (SELECT COALESCE(max(period_end), now() - interval '1 day') "
            "                     FROM sage.briefings)");

        /* 4. Actions taken */
        actions = spi_query_simple(
            "SELECT action_type, sql_executed, outcome "
            "FROM sage.action_log "
            "WHERE executed_at > (SELECT COALESCE(max(period_end), now() - interval '1 day') "
            "                     FROM sage.briefings)");

        /* 5. System health snapshot */
        system_health = spi_query_simple(
            "SELECT data FROM sage.snapshots "
            "WHERE category = 'system' ORDER BY collected_at DESC LIMIT 1");

        /* 6. Self health */
        sage_health = spi_query_simple(
            "SELECT data FROM sage.snapshots "
            "WHERE category = 'sage_health' ORDER BY collected_at DESC LIMIT 1");
    }
    PG_CATCH();
    {
        SPI_finish();
        PG_RE_THROW();
    }
    PG_END_TRY();

    /* Attempt LLM-enhanced briefing */
    if (use_llm && sage_llm_available())
    {
        StringInfoData  sys_prompt;
        StringInfoData  usr_prompt;
        int             tokens_used = 0;

        initStringInfo(&sys_prompt);
        appendStringInfoString(&sys_prompt,
            "You are pg_sage, an autonomous PostgreSQL DBA agent. "
            "Generate a concise daily health briefing. Focus on what "
            "changed, what needs attention, and recommended actions. "
            "Use clear section headers. Keep it under 500 words.");

        initStringInfo(&usr_prompt);
        appendStringInfo(&usr_prompt,
            "## Open Findings Summary\n"
            "Critical: %d | Warning: %d | Info: %d\n\n",
            critical_count, warning_count, info_count);

        if (new_findings && new_findings[0] != '\0')
            appendStringInfo(&usr_prompt, "## New Findings\n%s\n", new_findings);
        else
            appendStringInfoString(&usr_prompt, "## New Findings\nNone\n\n");

        if (resolved && resolved[0] != '\0')
            appendStringInfo(&usr_prompt, "## Resolved\n%s\n", resolved);

        if (actions && actions[0] != '\0')
            appendStringInfo(&usr_prompt, "## Actions Taken\n%s\n", actions);

        if (system_health && system_health[0] != '\0')
            appendStringInfo(&usr_prompt, "## System Health\n%s\n", system_health);

        if (sage_health && sage_health[0] != '\0')
            appendStringInfo(&usr_prompt, "## Sage Health\n%s\n", sage_health);

        content = sage_llm_call(sys_prompt.data, usr_prompt.data, 1024, &tokens_used);

        pfree(sys_prompt.data);
        pfree(usr_prompt.data);

        if (content != NULL)
        {
            appendStringInfoString(&briefing, content);
            pfree(content);
            goto store_briefing;
        }
        /* Fall through to structured output if LLM failed */
    }

    /* Structured output (fallback or no LLM) */
    now_ts = GetCurrentTimestamp();
    snprintf(ts_buf, sizeof(ts_buf), "%s",
             timestamptz_to_str(now_ts));

    appendStringInfo(&briefing,
        "=== pg_sage Daily Briefing ===\n"
        "Generated: %s\n\n", ts_buf);

    /* Health Summary */
    appendStringInfo(&briefing,
        "## Health Summary\n"
        "Critical: %d | Warning: %d | Info: %d\n\n",
        critical_count, warning_count, info_count);

    /* New Findings */
    appendStringInfoString(&briefing, "## New Findings\n");
    if (new_findings && new_findings[0] != '\0')
    {
        /* Format each line as a bullet */
        char *line = strtok(new_findings, "\n");

        while (line != NULL)
        {
            if (line[0] != '\0')
                appendStringInfo(&briefing, "- %s\n", line);
            line = strtok(NULL, "\n");
        }
    }
    else
    {
        appendStringInfoString(&briefing, "- No new findings\n");
    }
    appendStringInfoChar(&briefing, '\n');

    /* Resolved */
    appendStringInfoString(&briefing, "## Resolved\n");
    if (resolved && resolved[0] != '\0')
    {
        char *line = strtok(resolved, "\n");

        while (line != NULL)
        {
            if (line[0] != '\0')
                appendStringInfo(&briefing, "- %s\n", line);
            line = strtok(NULL, "\n");
        }
    }
    else
    {
        appendStringInfoString(&briefing, "- None\n");
    }
    appendStringInfoChar(&briefing, '\n');

    /* Actions Taken */
    appendStringInfoString(&briefing, "## Actions Taken\n");
    if (actions && actions[0] != '\0')
    {
        char *line = strtok(actions, "\n");

        while (line != NULL)
        {
            if (line[0] != '\0')
                appendStringInfo(&briefing, "- %s\n", line);
            line = strtok(NULL, "\n");
        }
    }
    else
    {
        appendStringInfoString(&briefing, "- None\n");
    }
    appendStringInfoChar(&briefing, '\n');

    /* System Metrics */
    appendStringInfoString(&briefing, "## System Metrics\n");
    if (system_health && system_health[0] != '\0')
        appendStringInfoString(&briefing, system_health);
    else
        appendStringInfoString(&briefing, "- No system snapshot available\n");
    appendStringInfoChar(&briefing, '\n');

store_briefing:

    /* Store the briefing in sage.briefings using parameterized query.
     * Use a savepoint so errors don't abort the caller's transaction. */
    SPI_execute("SAVEPOINT _sage_store_briefing", false, 0);
    PG_TRY();
    {
        static const char *insert_sql =
            "INSERT INTO sage.briefings (period_start, period_end, mode, content_text, content_json, delivery_status) "
            "VALUES (COALESCE((SELECT max(period_end) FROM sage.briefings), now() - interval '1 day'), "
            "        now(), 'on_demand', $1, '{}'::jsonb, '{\"status\": \"generated\"}'::jsonb)";

        Oid     argtypes[1] = {TEXTOID};
        Datum   values[1];
        char    nulls[1] = {' '};

        values[0] = CStringGetTextDatum(briefing.data);

        SPI_execute_with_args(insert_sql, 1, argtypes, values, nulls, false, 0);
        SPI_execute("RELEASE SAVEPOINT _sage_store_briefing", false, 0);
    }
    PG_CATCH();
    {
        FlushErrorState();
        SPI_execute("ROLLBACK TO SAVEPOINT _sage_store_briefing", false, 0);
        SPI_execute("RELEASE SAVEPOINT _sage_store_briefing", false, 0);
        elog(WARNING, "pg_sage: failed to store briefing in sage.briefings");
    }
    PG_END_TRY();

    SPI_finish();

    /*
     * Do NOT pfree new_findings, resolved, actions, system_health, sage_health
     * here — they were allocated in SPI memory context and already freed by
     * SPI_finish() above.  Explicit pfree would be a double-free.
     */

    pfree(data_buf.data);

    return briefing.data;
}

/* ----------------------------------------------------------------
 * sage_deliver_briefing
 *
 * Deliver briefing content to configured channels.
 * ---------------------------------------------------------------- */
void
sage_deliver_briefing(const char *content, const char *content_json)
{
    char   *channels;
    char   *token;
    char   *saveptr;

    if (content == NULL || content[0] == '\0')
        return;

    if (sage_briefing_channels == NULL || sage_briefing_channels[0] == '\0')
    {
        /* Default: just log to stdout */
        elog(LOG, "pg_sage briefing:\n%s", content);
        return;
    }

    channels = pstrdup(sage_briefing_channels);
    token = strtok_r(channels, ",", &saveptr);

    while (token != NULL)
    {
        /* Trim leading whitespace */
        while (*token == ' ')
            token++;

        if (strcmp(token, "stdout") == 0)
        {
            elog(LOG, "pg_sage briefing:\n%s", content);
        }
        else if (strcmp(token, "table") == 0)
        {
            /* Already stored by sage_generate_briefing, update status */
            PG_TRY();
            {
                SPI_connect();
                SPI_execute(
                    "UPDATE sage.briefings "
                    "SET delivery_status = '{\"status\": \"delivered\"}'::jsonb "
                    "WHERE id = (SELECT max(id) FROM sage.briefings)",
                    false, 0);
                SPI_finish();
            }
            PG_CATCH();
            {
                EmitErrorReport();
                FlushErrorState();
            }
            PG_END_TRY();
        }
        else if (strcmp(token, "slack") == 0)
        {
            /* HTTP POST to Slack webhook */
            if (sage_slack_webhook_url == NULL ||
                sage_slack_webhook_url[0] == '\0')
            {
                elog(WARNING, "pg_sage: slack channel configured but "
                     "sage.slack_webhook_url is not set");
            }
            else
            {
                CURL               *curl;
                CURLcode            res;
                struct curl_slist  *headers = NULL;
                StringInfoData      payload;
                StringInfoData      escaped;
                const char         *p;

                curl = curl_easy_init();
                if (curl != NULL)
                {
                    /* JSON-escape the content for Slack payload */
                    initStringInfo(&escaped);
                    for (p = content; *p; p++)
                    {
                        switch (*p)
                        {
                            case '"':
                                appendStringInfoString(&escaped, "\\\"");
                                break;
                            case '\\':
                                appendStringInfoString(&escaped, "\\\\");
                                break;
                            case '\n':
                                appendStringInfoString(&escaped, "\\n");
                                break;
                            case '\r':
                                appendStringInfoString(&escaped, "\\r");
                                break;
                            case '\t':
                                appendStringInfoString(&escaped, "\\t");
                                break;
                            default:
                                appendStringInfoChar(&escaped, *p);
                                break;
                        }
                    }

                    initStringInfo(&payload);
                    appendStringInfo(&payload, "{\"text\":\"%s\"}", escaped.data);
                    pfree(escaped.data);

                    headers = curl_slist_append(headers,
                                               "Content-Type: application/json");

                    curl_easy_setopt(curl, CURLOPT_URL,
                                    sage_slack_webhook_url);
                    curl_easy_setopt(curl, CURLOPT_HTTPHEADER, headers);
                    curl_easy_setopt(curl, CURLOPT_POSTFIELDS, payload.data);
                    curl_easy_setopt(curl, CURLOPT_TIMEOUT, 10L);
                    curl_easy_setopt(curl, CURLOPT_WRITEFUNCTION,
                                    slack_write_callback);
                    curl_easy_setopt(curl, CURLOPT_NOSIGNAL, 1L);

                    res = curl_easy_perform(curl);
                    if (res != CURLE_OK)
                    {
                        elog(WARNING, "pg_sage: Slack delivery failed: %s",
                             curl_easy_strerror(res));
                    }
                    else
                    {
                        elog(DEBUG1, "pg_sage: briefing delivered to Slack");
                    }

                    curl_slist_free_all(headers);
                    curl_easy_cleanup(curl);
                    pfree(payload.data);
                }
            }
        }
        else if (strcmp(token, "email") == 0)
        {
            elog(WARNING, "pg_sage: email delivery is not implemented "
                 "in Phase 0.1; use 'slack' or 'stdout' instead");
        }
        else if (strcmp(token, "notify") == 0)
        {
            /* Use NOTIFY/LISTEN */
            PG_TRY();
            {
                StringInfoData  notify_sql;
                char           *escaped_payload;

                /*
                 * Use quote_literal_cstr to properly escape the payload
                 * for SQL inclusion (handles single quotes AND backslashes).
                 */
                escaped_payload = quote_literal_cstr(content);

                /* NOTIFY payload is limited to ~8000 bytes; truncate if needed */
                if (strlen(escaped_payload) > 7800)
                    escaped_payload[7800] = '\0';

                initStringInfo(&notify_sql);
                appendStringInfo(&notify_sql,
                    "NOTIFY sage_briefing, %s", escaped_payload);

                SPI_connect();
                SPI_execute(notify_sql.data, false, 0);
                SPI_finish();

                pfree(notify_sql.data);
                pfree(escaped_payload);
            }
            PG_CATCH();
            {
                EmitErrorReport();
                FlushErrorState();
                elog(WARNING, "pg_sage: NOTIFY delivery failed");
            }
            PG_END_TRY();
        }
        else
        {
            elog(WARNING, "pg_sage: unknown briefing channel '%s'", token);
        }

        token = strtok_r(NULL, ",", &saveptr);
    }

    pfree(channels);
}

/* ----------------------------------------------------------------
 * sage_briefing (SQL-callable)
 *
 * On-demand briefing: SELECT sage.sage_briefing();
 * ---------------------------------------------------------------- */
PG_FUNCTION_INFO_V1(sage_briefing);

Datum
sage_briefing(PG_FUNCTION_ARGS)
{
    char   *content;
    bool    use_llm;

    use_llm = sage_llm_available();
    content = sage_generate_briefing(use_llm);

    if (content == NULL || content[0] == '\0')
        PG_RETURN_NULL();

    PG_RETURN_TEXT_P(cstring_to_text(content));
}

/* ----------------------------------------------------------------
 * sage_diagnose (SQL-callable)
 *
 * Interactive diagnostic: SELECT sage.sage_diagnose('why is orders slow?');
 * Implements a ReAct loop when LLM is available.
 * ---------------------------------------------------------------- */
PG_FUNCTION_INFO_V1(sage_diagnose);

Datum
sage_diagnose(PG_FUNCTION_ARGS)
{
    text           *question_text;
    char           *question;
    char           *context;
    StringInfoData  result;
    bool            is_named_object = false;
    char           *object_name = NULL;

    if (PG_ARGISNULL(0))
        ereport(ERROR,
                (errcode(ERRCODE_INVALID_PARAMETER_VALUE),
                 errmsg("question must not be NULL")));

    question_text = PG_GETARG_TEXT_PP(0);
    question = text_to_cstring(question_text);

    initStringInfo(&result);

    /*
     * Determine if this is a named-object question or a system-wide one.
     * Simple heuristic: look for common table-related keywords followed
     * by an identifier.
     */
    {
        const char *keywords[] = {
            "table ", "index ", "relation ", "on ", "for ", NULL
        };
        int k;
        char *lower_q = pstrdup(question);
        char *p;

        /* Lowercase for matching */
        for (p = lower_q; *p; p++)
            *p = pg_tolower((unsigned char) *p);

        for (k = 0; keywords[k] != NULL; k++)
        {
            char *found = strstr(lower_q, keywords[k]);

            if (found != NULL)
            {
                char *name_start;
                char *name_end;
                int   name_len;

                /* Position in original string */
                name_start = question + (found - lower_q) + strlen(keywords[k]);

                /* Skip leading whitespace */
                while (*name_start == ' ' || *name_start == '"')
                    name_start++;

                /* Find end of identifier */
                name_end = name_start;
                while (*name_end && *name_end != ' ' && *name_end != '?' &&
                       *name_end != ',' && *name_end != '"' &&
                       *name_end != '\n' && *name_end != '.')
                    name_end++;

                name_len = name_end - name_start;
                if (name_len > 0 && name_len < 128)
                {
                    object_name = pnstrdup(name_start, name_len);
                    is_named_object = true;
                    break;
                }
            }
        }

        pfree(lower_q);
    }

    /* Assemble context */
    if (is_named_object && object_name != NULL)
        context = sage_assemble_context_named(object_name);
    else
        context = sage_assemble_context_system();

    /* If LLM available, run ReAct diagnostic loop */
    if (sage_llm_available())
    {
        StringInfoData  conversation;
        char           *system_prompt;
        char           *llm_response;
        int             step;
        int             tokens_used = 0;
        int             total_tokens = 0;
        int             spi_conn;

        system_prompt =
            "You are pg_sage, an autonomous PostgreSQL DBA diagnostic agent. "
            "You are analyzing a PostgreSQL database. "
            "You may request additional diagnostic SQL queries using:\n"
            "ACTION: <sql query here>\n\n"
            "Rules for ACTION queries:\n"
            "- Only SELECT queries (read-only)\n"
            "- No DDL or DML\n"
            "- Keep queries focused and efficient\n"
            "- Use the correct column names from PostgreSQL catalog views. "
            "For example, pg_stat_user_indexes uses 'indexrelname' (not 'indexname').\n\n"
            "When you have enough information, provide your conclusion using:\n"
            "CONCLUSION: <your analysis and recommendations>\n\n"
            "If a query fails, you will see the error message. Fix the query and try again.\n\n"
            "Be specific, reference actual data, and provide actionable recommendations.";

        /* SPI connection is opened per-query inside subtransactions
         * to properly isolate failures. */
        (void) spi_conn; /* suppress unused variable warning */

        initStringInfo(&conversation);
        appendStringInfo(&conversation,
            "## Database Context\n%s\n\n"
            "## Question\n%s",
            context, question);

        /* ReAct loop */
        for (step = 0; step < sage_react_max_steps; step++)
        {
            char *action_start;
            char *conclusion_start;

            llm_response = sage_llm_call(system_prompt, conversation.data,
                                         2048, &tokens_used);
            total_tokens += tokens_used;

            if (llm_response == NULL)
            {
                appendStringInfoString(&result,
                    "[LLM call failed during diagnostic loop]\n\n");
                break;
            }

            /* Check for CONCLUSION */
            conclusion_start = strstr(llm_response, "CONCLUSION:");
            if (conclusion_start != NULL)
            {
                conclusion_start += strlen("CONCLUSION:");
                while (*conclusion_start == ' ')
                    conclusion_start++;

                appendStringInfoString(&result, conclusion_start);
                pfree(llm_response);
                break;
            }

            /* Check for ACTION directive */
            action_start = strstr(llm_response, "ACTION:");
            if (action_start != NULL)
            {
                char   *sql_start;
                char   *sql_end;
                char   *diag_sql;
                int     sql_len;
                char   *sql_result;

                sql_start = action_start + strlen("ACTION:");
                while (*sql_start == ' ' || *sql_start == '\n')
                    sql_start++;

                /* Find end of SQL (next newline pair or end) */
                sql_end = sql_start;
                while (*sql_end && !(*sql_end == '\n' &&
                       (*(sql_end + 1) == '\n' || *(sql_end + 1) == '\0')))
                    sql_end++;

                sql_len = sql_end - sql_start;
                if (sql_len <= 0 || sql_len > 4096)
                {
                    appendStringInfo(&conversation,
                        "\n\n## Step %d Result\n"
                        "Error: Invalid SQL query length\n",
                        step + 1);
                    pfree(llm_response);
                    continue;
                }

                diag_sql = pnstrdup(sql_start, sql_len);

                /* Ensure the query is read-only */
                {
                    char *lower_sql = pstrdup(diag_sql);
                    char *p;

                    for (p = lower_sql; *p; p++)
                        *p = pg_tolower((unsigned char) *p);

                    /* Skip leading whitespace */
                    p = lower_sql;
                    while (*p == ' ' || *p == '\t' || *p == '\n')
                        p++;

                    if (strncmp(p, "select", 6) != 0 &&
                        strncmp(p, "with", 4) != 0 &&
                        strncmp(p, "explain", 7) != 0 &&
                        strncmp(p, "show", 4) != 0)
                    {
                        appendStringInfo(&conversation,
                            "\n\n## Step %d Result\n"
                            "Error: Only SELECT/WITH/EXPLAIN/SHOW queries are allowed.\n",
                            step + 1);
                        pfree(lower_sql);
                        pfree(diag_sql);
                        pfree(llm_response);
                        continue;
                    }
                    pfree(lower_sql);
                }

                /* Execute the diagnostic query inside a subtransaction
                 * with its own SPI connection.  This is the standard
                 * PostgreSQL pattern for isolating errors in C extension
                 * code: subtxn + SPI_connect/SPI_finish per attempt. */
                {
                    MemoryContext  oldcxt = CurrentMemoryContext;
                    ResourceOwner oldowner = CurrentResourceOwner;

                    BeginInternalSubTransaction(NULL);
                    PG_TRY();
                    {
                        int inner_conn = SPI_connect();
                        if (inner_conn == SPI_OK_CONNECT)
                        {
                            char *tmp = spi_query_simple(diag_sql);
                            /* Copy to outer context before SPI_finish
                             * frees SPI memory */
                            MemoryContextSwitchTo(oldcxt);
                            sql_result = tmp ? pstrdup(tmp) : NULL;
                            SPI_finish();
                        }
                        else
                        {
                            MemoryContextSwitchTo(oldcxt);
                            sql_result = pstrdup("[SPI connection failed]");
                        }
                        ReleaseCurrentSubTransaction();
                        MemoryContextSwitchTo(oldcxt);
                        CurrentResourceOwner = oldowner;
                    }
                    PG_CATCH();
                    {
                        ErrorData *edata;
                        StringInfoData errbuf;

                        MemoryContextSwitchTo(oldcxt);
                        edata = CopyErrorData();
                        FlushErrorState();

                        RollbackAndReleaseCurrentSubTransaction();
                        MemoryContextSwitchTo(oldcxt);
                        CurrentResourceOwner = oldowner;

                        /* Feed the error back to the LLM so it can
                         * correct the query and retry */
                        initStringInfo(&errbuf);
                        appendStringInfo(&errbuf,
                            "[Query error: %s]",
                            edata->message ? edata->message
                                           : "unknown error");
                        sql_result = errbuf.data;
                        FreeErrorData(edata);
                    }
                    PG_END_TRY();
                }

                /* Append result to conversation */
                appendStringInfo(&conversation,
                    "\n\n## Step %d: Executed Query\n```sql\n%s\n```\n"
                    "## Result\n%s\n",
                    step + 1,
                    diag_sql,
                    (sql_result && sql_result[0] != '\0') ?
                        sql_result : "(no rows returned)");

                pfree(diag_sql);
                if (sql_result)
                    pfree(sql_result);
            }
            else
            {
                /*
                 * No ACTION and no CONCLUSION -- treat the whole
                 * response as the conclusion.
                 */
                appendStringInfoString(&result, llm_response);
                pfree(llm_response);
                break;
            }

            pfree(llm_response);
        }

        /* If we exhausted steps without a conclusion */
        if (result.len == 0)
        {
            appendStringInfoString(&result,
                "[Diagnostic loop reached maximum steps without conclusion. "
                "Partial analysis may be available in the PostgreSQL log.]\n");
        }

        appendStringInfo(&result, "\n\n---\n_Tokens used: %d_", total_tokens);

        pfree(conversation.data);
    }
    else
    {
        /*
         * No LLM: return relevant findings as structured text.
         */
        int spi_conn;

        appendStringInfo(&result,
            "=== pg_sage Diagnostic Report ===\n"
            "Question: %s\n\n", question);

        if (is_named_object && object_name != NULL)
        {
            appendStringInfo(&result,
                "## Object: %s\n\n"
                "## Context\n%s\n\n"
                "## Relevant Findings\n",
                object_name, context);

            spi_conn = SPI_connect();
            if (spi_conn == SPI_OK_CONNECT)
            {
                StringInfoData  fq;
                char           *findings_text;
                Oid             argtypes[1] = { TEXTOID };
                Datum           values[1];
                char            nulls[1] = { ' ' };
                int             ret;

                values[0] = CStringGetTextDatum(object_name);

                initStringInfo(&fq);
                appendStringInfoString(&fq,
                    "SELECT severity, title, recommendation, "
                    "       COALESCE(recommended_sql, '') as sql "
                    "FROM sage.findings "
                    "WHERE object_identifier LIKE '%%' || $1 || '%%' "
                    "  AND status = 'open' "
                    "ORDER BY CASE severity "
                    "  WHEN 'critical' THEN 0 "
                    "  WHEN 'warning' THEN 1 ELSE 2 END");

                ret = SPI_execute_with_args(fq.data, 1, argtypes,
                                            values, nulls, true, 0);

                if (ret == SPI_OK_SELECT && SPI_tuptable != NULL)
                {
                    int i;

                    for (i = 0; i < (int) SPI_processed; i++)
                    {
                        char *sev   = SPI_getvalue(SPI_tuptable->vals[i],
                                                    SPI_tuptable->tupdesc, 1);
                        char *title = SPI_getvalue(SPI_tuptable->vals[i],
                                                    SPI_tuptable->tupdesc, 2);
                        char *rec   = SPI_getvalue(SPI_tuptable->vals[i],
                                                    SPI_tuptable->tupdesc, 3);
                        char *sql   = SPI_getvalue(SPI_tuptable->vals[i],
                                                    SPI_tuptable->tupdesc, 4);

                        appendStringInfo(&result, "- [%s] %s\n  Recommendation: %s\n",
                                         sev ? sev : "?",
                                         title ? title : "untitled",
                                         rec ? rec : "none");
                        if (sql && sql[0] != '\0')
                            appendStringInfo(&result, "  SQL: %s\n", sql);
                    }

                    if (SPI_processed == 0)
                        appendStringInfoString(&result, "- No open findings for this object.\n");
                }
                else
                {
                    appendStringInfoString(&result, "- Unable to query findings.\n");
                }

                pfree(fq.data);
                pfree(DatumGetPointer(values[0]));
                SPI_finish();
            }
        }
        else
        {
            appendStringInfo(&result,
                "## System-Wide Context\n%s\n\n"
                "## Note\n"
                "LLM is not available. Enable it (sage.llm_enabled = on) for "
                "interactive diagnostic analysis with follow-up queries.\n",
                context);
        }
    }

    pfree(context);
    pfree(question);

    PG_RETURN_TEXT_P(cstring_to_text(result.data));
}
