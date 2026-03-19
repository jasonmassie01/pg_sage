/*
 * action_executor.c — Tier 3 Action Executor for pg_sage
 *
 * Executes recommended actions from findings based on trust level,
 * maintenance window, and risk classification.  Runs inside the
 * analyzer background worker (not its own worker) after all analysis
 * functions complete.
 *
 * AGPL-3.0 License
 */

#include "pg_sage.h"

#include <string.h>
#include <ctype.h>

#include "executor/spi.h"
#include "utils/timestamp.h"
#include "lib/stringinfo.h"
#include "utils/builtins.h"
#include "catalog/pg_type.h"

/* ----------------------------------------------------------------
 * Action risk levels
 * ---------------------------------------------------------------- */
typedef enum SageActionRisk
{
    SAGE_RISK_SAFE = 0,     /* Low risk: drop unused index, vacuum tune */
    SAGE_RISK_MODERATE,     /* Medium: CREATE INDEX, REINDEX, config */
    SAGE_RISK_HIGH          /* High: everything else — log only */
} SageActionRisk;

/* Forward declarations */
static SageActionRisk classify_action_risk(const char *category,
                                           const char *severity,
                                           const char *sql);
static void sage_execute_action(int64 finding_id,
                                const char *category,
                                const char *severity,
                                const char *object_id,
                                const char *title,
                                const char *sql,
                                const char *rollback_sql);
static char *capture_before_state(const char *category,
                                  const char *object_id);

/* ----------------------------------------------------------------
 * Day-of-week parsing helpers for maintenance window
 * ---------------------------------------------------------------- */
static const char * const day_names[] = {
    "sun", "mon", "tue", "wed", "thu", "fri", "sat"
};

/*
 * parse_day_abbrev
 *
 * Parse a 3-letter day abbreviation (case-insensitive) and return
 * the day number (0=Sun..6=Sat).  Returns -1 on failure.
 * Advances *pos past the abbreviation on success.
 */
static int
parse_day_abbrev(const char *str, int *pos)
{
    char    buf[4];
    int     i;

    if (str[*pos] == '\0' || str[*pos + 1] == '\0' || str[*pos + 2] == '\0')
        return -1;

    buf[0] = tolower((unsigned char) str[*pos]);
    buf[1] = tolower((unsigned char) str[*pos + 1]);
    buf[2] = tolower((unsigned char) str[*pos + 2]);
    buf[3] = '\0';

    for (i = 0; i < 7; i++)
    {
        if (memcmp(buf, day_names[i], 3) == 0)
        {
            *pos += 3;
            return i;
        }
    }

    return -1;
}

/*
 * parse_hour_minute
 *
 * Parse "HH:MM" and return the hour (0-23).  Minutes are parsed
 * but only the hour is used for window comparisons.
 * Returns -1 on failure.  Advances *pos past the time string.
 */
static int
parse_hour_minute(const char *str, int *pos)
{
    int hour, minute;

    if (!isdigit((unsigned char) str[*pos]))
        return -1;

    hour = (str[*pos] - '0');
    (*pos)++;

    if (isdigit((unsigned char) str[*pos]))
    {
        hour = hour * 10 + (str[*pos] - '0');
        (*pos)++;
    }

    if (str[*pos] != ':')
        return -1;
    (*pos)++;

    if (!isdigit((unsigned char) str[*pos]) ||
        !isdigit((unsigned char) str[*pos + 1]))
        return -1;

    minute = (str[*pos] - '0') * 10 + (str[*pos + 1] - '0');
    *pos += 2;

    (void) minute;  /* currently unused — hour granularity only */

    if (hour < 0 || hour > 23)
        return -1;

    return hour;
}

/*
 * day_in_range
 *
 * Check if 'day' falls within [day_start..day_end] with wrap-around
 * through the week (e.g. Fri-Mon = 5,6,0,1).
 */
static bool
day_in_range(int day, int day_start, int day_end)
{
    if (day_start <= day_end)
        return (day >= day_start && day <= day_end);
    else
        return (day >= day_start || day <= day_end);
}

/*
 * hour_in_range
 *
 * Check if 'hour' falls within [hour_start..hour_end) with overnight
 * wrap-around (e.g. 22:00-06:00 = hours 22,23,0,1,2,3,4,5).
 */
static bool
hour_in_range(int hour, int hour_start, int hour_end)
{
    if (hour_start <= hour_end)
        return (hour >= hour_start && hour < hour_end);
    else
        return (hour >= hour_start || hour < hour_end);
}

/* ----------------------------------------------------------------
 * sage_check_maintenance_window
 *
 * Check whether the current time falls within the configured
 * maintenance window.
 *
 * Supported formats:
 *   - NULL or empty or '*' or 'always' => always in window
 *   - "Sun 02:00-06:00"               => specific day + hour range
 *   - "Sat-Sun 02:00-06:00"           => day range + hour range
 *   - "Mon-Fri 22:00-06:00"           => day range + overnight wrap
 *   - "02:00-06:00"                   => any day, hour range only
 *
 * Returns false on parse failure (safe default: no actions).
 * ---------------------------------------------------------------- */
bool
sage_check_maintenance_window(void)
{
    const char     *spec;
    int             pos;
    int             day_start = -1;
    int             day_end = -1;
    int             hour_start;
    int             hour_end;
    TimestampTz     now_ts;
    struct pg_tm    tm;
    fsec_t          fsec;
    int             tz;
    int             current_wday;
    int             current_hour;

    /* NULL or empty => always in window (backward compat: was false,
     * but '*' is the documented default in the GUC) */
    if (sage_maintenance_window == NULL ||
        sage_maintenance_window[0] == '\0')
        return false;

    spec = sage_maintenance_window;

    /* Wildcard / always */
    if (strcmp(spec, "*") == 0 ||
        pg_strcasecmp(spec, "always") == 0)
        return true;

    /* Get current local time */
    now_ts = GetCurrentTimestamp();
    if (timestamp2tm(now_ts, &tz, &tm, &fsec, NULL, NULL) != 0)
    {
        ereport(WARNING,
                (errmsg("pg_sage: cannot determine current time for "
                        "maintenance window check")));
        return false;
    }

    current_wday = tm.tm_wday;   /* 0=Sun .. 6=Sat */
    current_hour = tm.tm_hour;   /* 0..23 */

    /* Parse the spec string */
    pos = 0;

    /* Skip leading whitespace */
    while (spec[pos] == ' ')
        pos++;

    /* Try to parse a day-of-week at the current position */
    if (isalpha((unsigned char) spec[pos]))
    {
        day_start = parse_day_abbrev(spec, &pos);
        if (day_start < 0)
            goto parse_error;

        /* Check for day range (e.g. "Mon-Fri") */
        if (spec[pos] == '-')
        {
            pos++;
            day_end = parse_day_abbrev(spec, &pos);
            if (day_end < 0)
                goto parse_error;
        }
        else
        {
            /* Single day */
            day_end = day_start;
        }

        /* Expect whitespace before the hour range */
        if (spec[pos] != ' ')
            goto parse_error;
        while (spec[pos] == ' ')
            pos++;
    }

    /* Parse hour range: HH:MM-HH:MM */
    hour_start = parse_hour_minute(spec, &pos);
    if (hour_start < 0)
        goto parse_error;

    if (spec[pos] != '-')
        goto parse_error;
    pos++;

    hour_end = parse_hour_minute(spec, &pos);
    if (hour_end < 0)
        goto parse_error;

    /* Should be at end of string (allow trailing whitespace) */
    while (spec[pos] == ' ')
        pos++;
    if (spec[pos] != '\0')
        goto parse_error;

    /* Check day-of-week if specified */
    if (day_start >= 0)
    {
        if (!day_in_range(current_wday, day_start, day_end))
            return false;
    }

    /* Check hour range */
    return hour_in_range(current_hour, hour_start, hour_end);

parse_error:
    ereport(WARNING,
            (errmsg("pg_sage: unrecognized maintenance window format: \"%s\"",
                    sage_maintenance_window),
             errhint("Expected format: \"Sun 02:00-06:00\", "
                     "\"Mon-Fri 22:00-06:00\", \"02:00-06:00\", "
                     "\"*\", or \"always\".")));
    return false;
}

/* ----------------------------------------------------------------
 * sage_action_executor_run
 *
 * Main entry point — called once per analyzer cycle after all
 * analysis functions complete.  The caller provides an active SPI
 * connection and transaction.
 * ---------------------------------------------------------------- */
void
sage_action_executor_run(void)
{
    SageTrustLevel  trust;
    bool            stopped;
    int             nrows;
    int             i;

    /* 1. Check trust level — observation mode never executes */
    trust = sage_get_trust_level();
    if (trust == SAGE_TRUST_OBSERVATION)
    {
        ereport(DEBUG1,
                (errmsg("pg_sage action_executor: trust level is observation, skipping")));
        return;
    }

    /* 2. Check emergency stop */
    if (sage_state)
    {
        LWLockAcquire(sage_state->lock, LW_SHARED);
        stopped = sage_state->emergency_stopped;
        LWLockRelease(sage_state->lock);

        if (stopped)
        {
            ereport(DEBUG1,
                    (errmsg("pg_sage action_executor: emergency stopped, skipping")));
            return;
        }
    }

    /* 3. Check HA safety */
    if (!sage_is_safe_for_writes())
    {
        ereport(DEBUG1,
                (errmsg("pg_sage action_executor: not safe for writes, skipping")));
        return;
    }

    /* 4. Connect SPI and query actionable findings */
    SPI_connect();
    sage_spi_exec("SET LOCAL statement_timeout = '5000ms'", 0);

    nrows = sage_spi_exec(
        "SELECT f.id, f.category, f.severity, f.object_identifier, f.title, "
        "       f.recommended_sql, f.rollback_sql "
        "FROM sage.findings f "
        "WHERE f.status = 'open' "
        "  AND f.recommended_sql IS NOT NULL "
        "  AND f.recommended_sql != '' "
        "  AND f.acted_on_at IS NULL "
        "  AND (f.suppressed_until IS NULL OR f.suppressed_until < now()) "
        "ORDER BY "
        "  CASE f.severity WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END, "
        "  f.created_at ASC "
        "LIMIT 5",
        0);

    if (nrows <= 0)
    {
        SPI_finish();
        ereport(DEBUG1,
                (errmsg("pg_sage action_executor: no actionable findings")));
        return;
    }

    ereport(LOG,
            (errmsg("pg_sage action_executor: found %d actionable findings", nrows)));

    /* 5. Process each finding — pstrdup all SPI values before re-entering SPI */
    for (i = 0; i < nrows; i++)
    {
        int64   finding_id;
        char   *category;
        char   *severity;
        char   *object_id;
        char   *title;
        char   *sql;
        char   *rollback_sql;

        finding_id = sage_spi_getval_int64(i, 0);

        category = sage_spi_getval_str(i, 1);
        category = category ? pstrdup(category) : pstrdup("");

        severity = sage_spi_getval_str(i, 2);
        severity = severity ? pstrdup(severity) : pstrdup("");

        object_id = sage_spi_getval_str(i, 3);
        object_id = object_id ? pstrdup(object_id) : pstrdup("");

        title = sage_spi_getval_str(i, 4);
        title = title ? pstrdup(title) : pstrdup("");

        sql = sage_spi_getval_str(i, 5);
        sql = sql ? pstrdup(sql) : NULL;

        rollback_sql = sage_spi_isnull(i, 6) ? NULL : sage_spi_getval_str(i, 6);
        rollback_sql = rollback_sql ? pstrdup(rollback_sql) : NULL;

        /* Skip if somehow sql is NULL after all the WHERE filters */
        if (sql == NULL || sql[0] == '\0')
        {
            pfree(category);
            pfree(severity);
            pfree(object_id);
            pfree(title);
            continue;
        }

        PG_TRY();
        {
            sage_execute_action(finding_id, category, severity,
                                object_id, title, sql, rollback_sql);
        }
        PG_CATCH();
        {
            ErrorData *edata = CopyErrorData();

            FlushErrorState();
            ereport(WARNING,
                    (errmsg("pg_sage action_executor: failed executing action "
                            "for finding " INT64_FORMAT ": %s",
                            finding_id,
                            edata->message ? edata->message : "(no message)")));
            FreeErrorData(edata);
        }
        PG_END_TRY();

        pfree(category);
        pfree(severity);
        pfree(object_id);
        pfree(title);
        if (sql)
            pfree(sql);
        if (rollback_sql)
            pfree(rollback_sql);
    }

    SPI_finish();
}

/* ----------------------------------------------------------------
 * classify_action_risk
 *
 * Determine risk level of a proposed action:
 *   SAFE:     unused index DROP, autovacuum tuning, kill idle
 *   MODERATE: CREATE INDEX, REINDEX, config changes
 *   HIGH:     everything else
 * ---------------------------------------------------------------- */
static SageActionRisk
classify_action_risk(const char *category, const char *severity,
                     const char *sql)
{
    if (category == NULL || sql == NULL)
        return SAGE_RISK_HIGH;

    /* SAFE actions */
    if ((strcmp(category, "unused_index") == 0 ||
         strcmp(category, "duplicate_index") == 0) &&
        strstr(sql, "DROP") != NULL)
        return SAGE_RISK_SAFE;

    if (strcmp(category, "vacuum") == 0 ||
        strcmp(category, "autovacuum") == 0 ||
        strcmp(category, "vacuum_bloat_dead_tuples") == 0 ||
        strcmp(category, "vacuum_staleness") == 0)
        return SAGE_RISK_SAFE;

    if (strcmp(category, "idle_session") == 0 ||
        strcmp(category, "idle_transaction") == 0)
        return SAGE_RISK_SAFE;

    /* MODERATE actions */
    if (strstr(sql, "CREATE INDEX") != NULL)
        return SAGE_RISK_MODERATE;

    if (strstr(sql, "REINDEX") != NULL)
        return SAGE_RISK_MODERATE;

    if (strcmp(category, "config") == 0 ||
        strcmp(category, "configuration") == 0)
        return SAGE_RISK_MODERATE;

    /* Everything else is HIGH risk */
    return SAGE_RISK_HIGH;
}

/* ----------------------------------------------------------------
 * capture_before_state
 *
 * Snapshot relevant metrics before executing an action so we can
 * compare afterwards for rollback decisions.
 *
 * Returns a palloc'd JSON string, or NULL on failure.
 * ---------------------------------------------------------------- */
static char *
capture_before_state(const char *category, const char *object_id)
{
    StringInfoData  buf;
    int             ret;
    char           *mean_latency_str = NULL;
    char           *idx_scan_str = NULL;

    initStringInfo(&buf);

    /* Capture current mean query latency */
    ret = sage_spi_exec(
        "SELECT round(avg(mean_exec_time)::numeric, 4)::text "
        "FROM pg_stat_statements "
        "WHERE calls > 0",
        0);

    if (ret > 0 && !sage_spi_isnull(0, 0))
        mean_latency_str = pstrdup(sage_spi_getval_str(0, 0));

    /* For index-related actions, capture index scan count */
    if (category != NULL &&
        (strstr(category, "index") != NULL))
    {
        StringInfoData idx_sql;

        initStringInfo(&idx_sql);
        appendStringInfo(&idx_sql,
            "SELECT idx_scan::text FROM pg_stat_user_indexes "
            "WHERE schemaname || '.' || indexrelname = '%s' "
            "LIMIT 1",
            object_id ? object_id : "");

        ret = sage_spi_exec(idx_sql.data, 0);
        pfree(idx_sql.data);

        if (ret > 0 && !sage_spi_isnull(0, 0))
            idx_scan_str = pstrdup(sage_spi_getval_str(0, 0));
    }

    /* Build JSON object */
    appendStringInfoChar(&buf, '{');
    appendStringInfo(&buf, "\"captured_at\": \"now\"");

    if (mean_latency_str)
    {
        char *escaped = sage_escape_json_string(mean_latency_str);

        appendStringInfo(&buf, ", \"mean_query_latency_ms\": \"%s\"", escaped);
        pfree(escaped);
        pfree(mean_latency_str);
    }

    if (idx_scan_str)
    {
        char *escaped = sage_escape_json_string(idx_scan_str);

        appendStringInfo(&buf, ", \"idx_scan\": \"%s\"", escaped);
        pfree(escaped);
        pfree(idx_scan_str);
    }

    appendStringInfoChar(&buf, '}');

    return buf.data;
}

/* ----------------------------------------------------------------
 * sage_execute_action
 *
 * Execute a single recommended action from a finding.
 *
 * Steps:
 *   1. Classify action risk
 *   2. Check trust day and maintenance window eligibility
 *   3. Capture before_state metrics
 *   4. Execute SQL in a savepoint
 *   5. Log to sage.action_log
 *   6. Update the finding
 * ---------------------------------------------------------------- */
static void
sage_execute_action(int64 finding_id,
                    const char *category,
                    const char *severity,
                    const char *object_id,
                    const char *title,
                    const char *sql,
                    const char *rollback_sql)
{
    SageActionRisk  risk;
    SageTrustLevel  trust;
    int             trust_day;
    bool            in_maintenance;
    char           *before_state;
    int             ret;
    const char     *risk_label;

    if (sql == NULL || sql[0] == '\0')
        return;

    risk = classify_action_risk(category, severity, sql);
    trust = sage_get_trust_level();
    trust_day = sage_get_trust_day();
    in_maintenance = sage_check_maintenance_window();

    /* Map risk to label for logging */
    switch (risk)
    {
        case SAGE_RISK_SAFE:
            risk_label = "SAFE";
            break;
        case SAGE_RISK_MODERATE:
            risk_label = "MODERATE";
            break;
        default:
            risk_label = "HIGH";
            break;
    }

    /* ---- Gate: HIGH_RISK actions are never executed ---- */
    if (risk == SAGE_RISK_HIGH)
    {
        ereport(LOG,
                (errmsg("pg_sage action_executor: skipping HIGH risk action "
                        "for finding " INT64_FORMAT " (%s): %.128s",
                        finding_id, category, sql)));
        return;
    }

    /* ---- Gate: SAFE actions require trust >= ADVISORY, day 8+ ---- */
    if (risk == SAGE_RISK_SAFE)
    {
        if (trust < SAGE_TRUST_ADVISORY || trust_day < 8)
        {
            ereport(LOG,
                    (errmsg("pg_sage action_executor: skipping SAFE action "
                            "for finding " INT64_FORMAT " (trust=%d, day=%d)",
                            finding_id, (int) trust, trust_day)));
            return;
        }
    }

    /* ---- Gate: MODERATE actions require trust >= AUTONOMOUS, day 31+ ---- */
    if (risk == SAGE_RISK_MODERATE)
    {
        if (trust < SAGE_TRUST_AUTONOMOUS || trust_day < 31)
        {
            ereport(LOG,
                    (errmsg("pg_sage action_executor: skipping MODERATE action "
                            "for finding " INT64_FORMAT " (trust=%d, day=%d)",
                            finding_id, (int) trust, trust_day)));
            return;
        }

        /* MODERATE actions also require a maintenance window */
        if (!in_maintenance)
        {
            ereport(LOG,
                    (errmsg("pg_sage action_executor: skipping MODERATE action "
                            "for finding " INT64_FORMAT " — not in maintenance window",
                            finding_id)));
            return;
        }
    }

    ereport(LOG,
            (errmsg("pg_sage action_executor: executing %s action for "
                    "finding " INT64_FORMAT " (%s): %.200s",
                    risk_label, finding_id, category, sql)));

    /* 3. Capture before_state */
    before_state = capture_before_state(category, object_id);

    /* 4. Execute SQL in a savepoint */
    sage_spi_exec("SAVEPOINT _sage_action", 0);

    ret = sage_spi_exec(sql, 0);

    if (ret < 0)
    {
        /* Execution failed — rollback savepoint and log */
        sage_spi_exec("ROLLBACK TO SAVEPOINT _sage_action", 0);
        sage_spi_exec("RELEASE SAVEPOINT _sage_action", 0);

        ereport(WARNING,
                (errmsg("pg_sage action_executor: action SQL failed for "
                        "finding " INT64_FORMAT ": %.128s",
                        finding_id, sql)));

        /* Log the failure to action_log */
        {
            static const char *fail_log_sql =
                "INSERT INTO sage.action_log "
                "(action_type, finding_id, sql_executed, rollback_sql, "
                " before_state, outcome) "
                "VALUES ($1, $2, $3, $4, $5::jsonb, 'failure')";

            Oid     argtypes[5] = {TEXTOID, INT8OID, TEXTOID, TEXTOID, TEXTOID};
            Datum   values[5];
            char    nulls[5];

            memset(nulls, ' ', sizeof(nulls));

            values[0] = CStringGetTextDatum(category ? category : "unknown");
            values[1] = Int64GetDatum(finding_id);
            values[2] = CStringGetTextDatum(sql);

            if (rollback_sql != NULL)
                values[3] = CStringGetTextDatum(rollback_sql);
            else
            {
                values[3] = (Datum) 0;
                nulls[3] = 'n';
            }

            if (before_state != NULL)
                values[4] = CStringGetTextDatum(before_state);
            else
            {
                values[4] = (Datum) 0;
                nulls[4] = 'n';
            }

            SPI_execute_with_args(fail_log_sql, 5, argtypes, values, nulls,
                                  false, 0);
        }

        if (before_state)
            pfree(before_state);
        return;
    }

    /* Success — release savepoint */
    sage_spi_exec("RELEASE SAVEPOINT _sage_action", 0);

    /* 5. Log to sage.action_log and get the action_log id */
    {
        static const char *log_sql =
            "INSERT INTO sage.action_log "
            "(action_type, finding_id, sql_executed, rollback_sql, "
            " before_state, outcome) "
            "VALUES ($1, $2, $3, $4, $5::jsonb, 'success') "
            "RETURNING id";

        Oid     argtypes[5] = {TEXTOID, INT8OID, TEXTOID, TEXTOID, TEXTOID};
        Datum   values[5];
        char    nulls[5];
        int     log_ret;
        int64   action_log_id = 0;

        memset(nulls, ' ', sizeof(nulls));

        values[0] = CStringGetTextDatum(category ? category : "unknown");
        values[1] = Int64GetDatum(finding_id);
        values[2] = CStringGetTextDatum(sql);

        if (rollback_sql != NULL)
            values[3] = CStringGetTextDatum(rollback_sql);
        else
        {
            values[3] = (Datum) 0;
            nulls[3] = 'n';
        }

        if (before_state != NULL)
            values[4] = CStringGetTextDatum(before_state);
        else
        {
            values[4] = (Datum) 0;
            nulls[4] = 'n';
        }

        log_ret = SPI_execute_with_args(log_sql, 5, argtypes, values, nulls,
                                        false, 0);

        if (log_ret == SPI_OK_INSERT_RETURNING && SPI_processed > 0)
        {
            action_log_id = sage_spi_getval_int64(0, 0);
        }
        else
        {
            elog(WARNING, "pg_sage action_executor: failed to insert action_log "
                 "for finding " INT64_FORMAT " (SPI returned %d)",
                 finding_id, log_ret);
        }

        /* 6. Update the finding to mark it as acted on */
        if (action_log_id > 0)
        {
            static const char *update_sql =
                "UPDATE sage.findings "
                "SET acted_on_at = now(), status = 'acted_on', "
                "    action_log_id = $1 "
                "WHERE id = $2";

            Oid     upd_argtypes[2] = {INT8OID, INT8OID};
            Datum   upd_values[2];
            char    upd_nulls[2] = {' ', ' '};
            int     upd_ret;

            upd_values[0] = Int64GetDatum(action_log_id);
            upd_values[1] = Int64GetDatum(finding_id);

            upd_ret = SPI_execute_with_args(update_sql, 2, upd_argtypes,
                                            upd_values, upd_nulls, false, 0);
            if (upd_ret != SPI_OK_UPDATE)
            {
                elog(WARNING, "pg_sage action_executor: failed to update "
                     "finding " INT64_FORMAT " (SPI returned %d)",
                     finding_id, upd_ret);
            }
        }
        else
        {
            /* Fallback: update finding even without action_log_id */
            StringInfoData upd_buf;

            initStringInfo(&upd_buf);
            appendStringInfo(&upd_buf,
                "UPDATE sage.findings "
                "SET acted_on_at = now(), status = 'acted_on' "
                "WHERE id = " INT64_FORMAT,
                finding_id);

            sage_spi_exec(upd_buf.data, 0);
            pfree(upd_buf.data);
        }
    }

    ereport(LOG,
            (errmsg("pg_sage action_executor: successfully executed %s action "
                    "for finding " INT64_FORMAT " (%s)",
                    risk_label, finding_id, title ? title : "")));

    if (before_state)
        pfree(before_state);
}

/* ----------------------------------------------------------------
 * sage_rollback_check
 *
 * Check recent actions for performance regression.  Called at the
 * end of each analyzer cycle.
 *
 * For each recent successful action that has no after_state yet,
 * compare current mean query latency against before_state.  If
 * regression exceeds sage_rollback_threshold%, execute rollback_sql
 * and update the action_log.
 * ---------------------------------------------------------------- */
void
sage_rollback_check(void)
{
    StringInfoData  query;
    int             nrows;
    int             i;
    int             ret;
    double          current_latency;

    /* Check trust level — only act if at least advisory */
    if (sage_get_trust_level() == SAGE_TRUST_OBSERVATION)
        return;

    if (!sage_is_safe_for_writes())
        return;

    SPI_connect();
    sage_spi_exec("SET LOCAL statement_timeout = '5000ms'", 0);

    /* First, get current mean query latency */
    ret = sage_spi_exec(
        "SELECT round(avg(mean_exec_time)::numeric, 4) "
        "FROM pg_stat_statements "
        "WHERE calls > 0",
        0);

    if (ret <= 0 || sage_spi_isnull(0, 0))
    {
        SPI_finish();
        ereport(DEBUG1,
                (errmsg("pg_sage rollback_check: cannot read current latency")));
        return;
    }

    current_latency = sage_spi_getval_float(0, 0);

    if (current_latency <= 0.0)
        return;

    /* Query recent actions that need regression checking */
    initStringInfo(&query);
    appendStringInfo(&query,
        "SELECT a.id, a.finding_id, a.rollback_sql, "
        "       a.before_state->>'mean_query_latency_ms' as before_latency "
        "FROM sage.action_log a "
        "WHERE a.outcome = 'success' "
        "  AND a.after_state IS NULL "
        "  AND a.rollback_sql IS NOT NULL "
        "  AND a.rollback_sql != '' "
        "  AND a.executed_at > now() - interval '%d minutes' "
        "  AND a.executed_at < now() - interval '%d days' IS NOT TRUE "
        "ORDER BY a.executed_at ASC "
        "LIMIT 10",
        sage_rollback_window > 0 ? sage_rollback_window : 60,
        sage_rollback_cooldown > 0 ? sage_rollback_cooldown : 1);

    nrows = sage_spi_exec(query.data, 0);
    pfree(query.data);

    if (nrows <= 0)
    {
        SPI_finish();
        return;
    }

    /* Copy result data before re-entering SPI */
    {
        typedef struct PendingCheck
        {
            int64   action_id;
            int64   finding_id;
            char   *rollback_sql;
            char   *before_latency_str;
        } PendingCheck;

        PendingCheck   *checks;
        int             nchecks = nrows;

        checks = palloc(sizeof(PendingCheck) * nchecks);

        for (i = 0; i < nchecks; i++)
        {
            char *tmp;

            checks[i].action_id = sage_spi_getval_int64(i, 0);
            checks[i].finding_id = sage_spi_getval_int64(i, 1);

            tmp = sage_spi_getval_str(i, 2);
            checks[i].rollback_sql = tmp ? pstrdup(tmp) : NULL;

            tmp = sage_spi_getval_str(i, 3);
            checks[i].before_latency_str = tmp ? pstrdup(tmp) : NULL;
        }

        for (i = 0; i < nchecks; i++)
        {
            double  before_latency;
            double  pct_change;
            int     threshold;

            if (checks[i].before_latency_str == NULL ||
                checks[i].rollback_sql == NULL)
            {
                /* Record after_state even if we can't compare */
                StringInfoData upd;

                initStringInfo(&upd);
                appendStringInfo(&upd,
                    "UPDATE sage.action_log "
                    "SET after_state = jsonb_build_object("
                    "  'mean_query_latency_ms', '%f'::text, "
                    "  'checked_at', now()::text) "
                    "WHERE id = " INT64_FORMAT,
                    current_latency, checks[i].action_id);

                sage_spi_exec(upd.data, 0);
                pfree(upd.data);
                goto next_check;
            }

            before_latency = strtod(checks[i].before_latency_str, NULL);
            if (before_latency <= 0.0)
                goto next_check;

            threshold = sage_rollback_threshold > 0 ? sage_rollback_threshold : 20;
            pct_change = ((current_latency - before_latency) / before_latency) * 100.0;

            if (pct_change > (double) threshold)
            {
                /* Regression detected — execute rollback */
                ereport(WARNING,
                        (errmsg("pg_sage rollback_check: regression detected "
                                "for action " INT64_FORMAT " (%.1f%% increase, "
                                "threshold %d%%), rolling back",
                                checks[i].action_id, pct_change, threshold)));

                sage_spi_exec("SAVEPOINT _sage_rollback", 0);

                ret = sage_spi_exec(checks[i].rollback_sql, 0);

                if (ret < 0)
                {
                    sage_spi_exec("ROLLBACK TO SAVEPOINT _sage_rollback", 0);
                    sage_spi_exec("RELEASE SAVEPOINT _sage_rollback", 0);

                    ereport(WARNING,
                            (errmsg("pg_sage rollback_check: rollback SQL failed "
                                    "for action " INT64_FORMAT,
                                    checks[i].action_id)));

                    /* Update action_log with failure */
                    {
                        StringInfoData upd;

                        initStringInfo(&upd);
                        appendStringInfo(&upd,
                            "UPDATE sage.action_log "
                            "SET outcome = 'rollback_failed', "
                            "    after_state = jsonb_build_object("
                            "      'mean_query_latency_ms', '%f'::text, "
                            "      'regression_pct', '%f'::text, "
                            "      'checked_at', now()::text) "
                            "WHERE id = " INT64_FORMAT,
                            current_latency, pct_change,
                            checks[i].action_id);

                        sage_spi_exec(upd.data, 0);
                        pfree(upd.data);
                    }
                }
                else
                {
                    sage_spi_exec("RELEASE SAVEPOINT _sage_rollback", 0);

                    ereport(LOG,
                            (errmsg("pg_sage rollback_check: successfully "
                                    "rolled back action " INT64_FORMAT,
                                    checks[i].action_id)));

                    /* Update action_log */
                    {
                        StringInfoData upd;

                        initStringInfo(&upd);
                        appendStringInfo(&upd,
                            "UPDATE sage.action_log "
                            "SET outcome = 'rolled_back', "
                            "    after_state = jsonb_build_object("
                            "      'mean_query_latency_ms', '%f'::text, "
                            "      'regression_pct', '%f'::text, "
                            "      'rolled_back_at', now()::text) "
                            "WHERE id = " INT64_FORMAT,
                            current_latency, pct_change,
                            checks[i].action_id);

                        sage_spi_exec(upd.data, 0);
                        pfree(upd.data);
                    }

                    /* Re-open the finding */
                    {
                        StringInfoData reopen;

                        initStringInfo(&reopen);
                        appendStringInfo(&reopen,
                            "UPDATE sage.findings "
                            "SET status = 'open', acted_on_at = NULL, "
                            "    action_log_id = NULL "
                            "WHERE id = " INT64_FORMAT,
                            checks[i].finding_id);

                        sage_spi_exec(reopen.data, 0);
                        pfree(reopen.data);
                    }
                }
            }
            else
            {
                /* No regression — record after_state and move on */
                StringInfoData upd;

                initStringInfo(&upd);
                appendStringInfo(&upd,
                    "UPDATE sage.action_log "
                    "SET after_state = jsonb_build_object("
                    "  'mean_query_latency_ms', '%f'::text, "
                    "  'regression_pct', '%f'::text, "
                    "  'checked_at', now()::text) "
                    "WHERE id = " INT64_FORMAT,
                    current_latency, pct_change,
                    checks[i].action_id);

                sage_spi_exec(upd.data, 0);
                pfree(upd.data);
            }

    next_check:
            if (checks[i].rollback_sql)
                pfree(checks[i].rollback_sql);
            if (checks[i].before_latency_str)
                pfree(checks[i].before_latency_str);
        }

        pfree(checks);
    }

    SPI_finish();
}
