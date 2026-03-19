/*
 * pg_sage.c — Main entry point for the pg_sage extension
 *
 * Registers GUCs, shared memory, background workers, and
 * SQL-callable control functions.
 *
 * AGPL-3.0 License
 */

#include "pg_sage.h"

#include <curl/curl.h>

PG_MODULE_MAGIC;

/* ----------------------------------------------------------------
 * Shared state pointer (lives in shmem after startup)
 * ---------------------------------------------------------------- */
SageSharedState *sage_state = NULL;

/* ----------------------------------------------------------------
 * Previous hook pointers (for chaining)
 * ---------------------------------------------------------------- */
static shmem_startup_hook_type prev_shmem_startup_hook = NULL;
#if PG_VERSION_NUM >= 150000
static shmem_request_hook_type prev_shmem_request_hook = NULL;
#endif

/* Database to connect workers to (defined in guc.c) */
extern char *sage_database;

/* ----------------------------------------------------------------
 * Forward declarations
 * ---------------------------------------------------------------- */
void _PG_init(void);

/* ----------------------------------------------------------------
 * sage_shmem_request — request shared memory space
 * ---------------------------------------------------------------- */
#if PG_VERSION_NUM >= 150000
void
sage_shmem_request(void)
{
    if (prev_shmem_request_hook)
        prev_shmem_request_hook();

    RequestAddinShmemSpace(MAXALIGN(sizeof(SageSharedState)));
    RequestNamedLWLockTranche("pg_sage", 1);
}
#endif

/* ----------------------------------------------------------------
 * sage_shmem_startup — initialise the shared state struct
 * ---------------------------------------------------------------- */
void
sage_shmem_startup(void)
{
    bool found;

    if (prev_shmem_startup_hook)
        prev_shmem_startup_hook();

    LWLockAcquire(AddinShmemInitLock, LW_EXCLUSIVE);

    sage_state = (SageSharedState *)
        ShmemInitStruct("pg_sage",
                        sizeof(SageSharedState),
                        &found);

    if (!found)
    {
        /* First time — zero-fill and set defaults */
        memset(sage_state, 0, sizeof(SageSharedState));

        sage_state->lock = &(GetNamedLWLockTranche("pg_sage"))->lock;

        sage_state->circuit_state          = SAGE_CIRCUIT_CLOSED;
        sage_state->consecutive_skips      = 0;
        sage_state->consecutive_successes  = 0;
        sage_state->last_circuit_change    = GetCurrentTimestamp();

        sage_state->llm_circuit_state         = SAGE_CIRCUIT_CLOSED;
        sage_state->llm_consecutive_failures  = 0;
        sage_state->llm_circuit_opened_at     = 0;

        sage_state->collector_running = false;
        sage_state->analyzer_running  = false;
        sage_state->briefing_running  = false;
        sage_state->last_collect_time = 0;
        sage_state->last_analyze_time = 0;
        sage_state->last_briefing_time = 0;

        sage_state->last_collector_duration_ms = 0.0;
        sage_state->last_analyzer_duration_ms  = 0.0;
        sage_state->sage_schema_bytes          = 0;

        sage_state->is_in_recovery   = false;
        sage_state->role_flip_count  = 0;

        sage_state->emergency_stopped = false;

        sage_state->trust_ramp_start = GetCurrentTimestamp();

        sage_state->llm_tokens_used_today = 0;
        sage_state->llm_day_of_year       = 0;

        /* Auto-explain ring buffer */
        pg_atomic_init_u32(&sage_state->explain_queue_head, 0);
        pg_atomic_init_u32(&sage_state->explain_queue_tail, 0);
        memset(sage_state->explain_queue, 0,
               sizeof(sage_state->explain_queue));
    }

    LWLockRelease(AddinShmemInitLock);
}

/* ----------------------------------------------------------------
 * register_sage_bgworker — helper to register one background worker
 * ---------------------------------------------------------------- */
static void
register_sage_bgworker(const char *name,
                       const char *function_name)
{
    BackgroundWorker worker;

    memset(&worker, 0, sizeof(BackgroundWorker));

    snprintf(worker.bgw_name, BGW_MAXLEN, "%s", name);
    snprintf(worker.bgw_type, BGW_MAXLEN, "%s", name);

    worker.bgw_flags = BGWORKER_SHMEM_ACCESS |
                       BGWORKER_BACKEND_DATABASE_CONNECTION;
    worker.bgw_start_time = BgWorkerStart_RecoveryFinished;
    worker.bgw_restart_time = 10;   /* seconds */
    worker.bgw_main_arg = (Datum) 0;
    worker.bgw_notify_pid = 0;

    snprintf(worker.bgw_library_name, BGW_MAXLEN, "pg_sage");
    snprintf(worker.bgw_function_name, BGW_MAXLEN, "%s", function_name);

    /*
     * Store the target database name in bgw_extra so the worker can
     * retrieve it at startup.  sage_database may not be set yet at
     * _PG_init time (GUCs are processed later), so we fall back to
     * "postgres".  Workers will re-read the GUC after connecting.
     */
    memset(worker.bgw_extra, 0, BGW_EXTRALEN);
    if (sage_database && sage_database[0] != '\0')
        strlcpy(worker.bgw_extra, sage_database, BGW_EXTRALEN);
    else
        strlcpy(worker.bgw_extra, "postgres", BGW_EXTRALEN);

    RegisterBackgroundWorker(&worker);
}

/* ----------------------------------------------------------------
 * _PG_init — extension load callback
 * ---------------------------------------------------------------- */
void
_PG_init(void)
{
    if (!process_shared_preload_libraries_in_progress)
    {
        ereport(WARNING,
                (errmsg("pg_sage must be loaded via shared_preload_libraries")));
        return;
    }

    /* 1. Register all GUCs */
    sage_guc_init();

    /* 2. Request shared memory */
#if PG_VERSION_NUM >= 150000
    prev_shmem_request_hook = shmem_request_hook;
    shmem_request_hook = sage_shmem_request;
#else
    RequestAddinShmemSpace(MAXALIGN(sizeof(SageSharedState)));
    /* On PG14, LWLock request must happen in _PG_init */
    RequestNamedLWLockTranche("pg_sage", 1);
#endif

    /* 3. Register shmem startup hook */
    prev_shmem_startup_hook = shmem_startup_hook;
    shmem_startup_hook = sage_shmem_startup;

    /* 4. Initialize libcurl globally (must happen once, before any threads) */
    curl_global_init(CURL_GLOBAL_ALL);

    /* 5. Install ExecutorEnd hook for passive EXPLAIN capture */
    sage_autoexplain_hook_init();

    /* 6. Warn if pg_stat_statements is not loaded */
    ereport(LOG,
            (errmsg("pg_sage %s: loading — ensure pg_stat_statements is "
                    "also in shared_preload_libraries", PG_SAGE_VERSION)));

    /* 7. Register background workers */
    register_sage_bgworker("pg_sage collector", "sage_collector_main");
    register_sage_bgworker("pg_sage analyzer",  "sage_analyzer_main");
    register_sage_bgworker("pg_sage briefing",  "sage_briefing_main");
}

/* ================================================================
 * SQL-callable functions
 * ================================================================ */

PG_FUNCTION_INFO_V1(sage_status);
PG_FUNCTION_INFO_V1(sage_emergency_stop);
PG_FUNCTION_INFO_V1(sage_resume);

/* ----------------------------------------------------------------
 * sage_status — return JSONB with current extension state
 * ---------------------------------------------------------------- */
Datum
sage_status(PG_FUNCTION_ARGS)
{
    JsonbParseState *state = NULL;
    JsonbValue *result;
    Jsonb      *jb;
    const char *circuit_str;
    const char *llm_circuit_str;

    /* Local copies of shared state — read under lock, then release */
    bool        local_enabled;
    bool        local_collector_running;
    bool        local_analyzer_running;
    bool        local_briefing_running;
    bool        local_emergency_stopped;
    TimestampTz local_last_collect_time;
    TimestampTz local_last_analyze_time;
    TimestampTz local_last_briefing_time;
    SageCircuitState local_circuit;
    SageCircuitState local_llm_circuit;

    if (!sage_state)
        ereport(ERROR,
                (errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
                 errmsg("pg_sage shared memory not initialised")));

    /* Copy all values under lock */
    LWLockAcquire(sage_state->lock, LW_SHARED);
    local_circuit              = sage_state->circuit_state;
    local_llm_circuit          = sage_state->llm_circuit_state;
    local_collector_running    = sage_state->collector_running;
    local_analyzer_running     = sage_state->analyzer_running;
    local_briefing_running     = sage_state->briefing_running;
    local_emergency_stopped    = sage_state->emergency_stopped;
    local_last_collect_time    = sage_state->last_collect_time;
    local_last_analyze_time    = sage_state->last_analyze_time;
    local_last_briefing_time   = sage_state->last_briefing_time;
    LWLockRelease(sage_state->lock);

    local_enabled = sage_enabled;

    /* Map circuit state to string */
    switch (local_circuit)
    {
        case SAGE_CIRCUIT_CLOSED:  circuit_str = "closed";  break;
        case SAGE_CIRCUIT_OPEN:    circuit_str = "open";     break;
        case SAGE_CIRCUIT_DORMANT: circuit_str = "dormant";  break;
        default:                   circuit_str = "unknown";  break;
    }

    switch (local_llm_circuit)
    {
        case SAGE_CIRCUIT_CLOSED:  llm_circuit_str = "closed";  break;
        case SAGE_CIRCUIT_OPEN:    llm_circuit_str = "open";     break;
        case SAGE_CIRCUIT_DORMANT: llm_circuit_str = "dormant";  break;
        default:                   llm_circuit_str = "unknown";  break;
    }

    pushJsonbValue(&state, WJB_BEGIN_OBJECT, NULL);

    /* version */
    {
        JsonbValue k, v;
        k.type = jbvString; k.val.string.val = "version";
        k.val.string.len = strlen("version");
        pushJsonbValue(&state, WJB_KEY, &k);

        v.type = jbvString; v.val.string.val = PG_SAGE_VERSION;
        v.val.string.len = strlen(PG_SAGE_VERSION);
        pushJsonbValue(&state, WJB_VALUE, &v);
    }

    /* enabled */
    {
        JsonbValue k, v;
        k.type = jbvString; k.val.string.val = "enabled";
        k.val.string.len = strlen("enabled");
        pushJsonbValue(&state, WJB_KEY, &k);

        v.type = jbvBool; v.val.boolean = local_enabled;
        pushJsonbValue(&state, WJB_VALUE, &v);
    }

    /* circuit_state */
    {
        JsonbValue k, v;
        k.type = jbvString; k.val.string.val = "circuit_state";
        k.val.string.len = strlen("circuit_state");
        pushJsonbValue(&state, WJB_KEY, &k);

        v.type = jbvString; v.val.string.val = pstrdup(circuit_str);
        v.val.string.len = strlen(circuit_str);
        pushJsonbValue(&state, WJB_VALUE, &v);
    }

    /* llm_circuit_state */
    {
        JsonbValue k, v;
        k.type = jbvString; k.val.string.val = "llm_circuit_state";
        k.val.string.len = strlen("llm_circuit_state");
        pushJsonbValue(&state, WJB_KEY, &k);

        v.type = jbvString; v.val.string.val = pstrdup(llm_circuit_str);
        v.val.string.len = strlen(llm_circuit_str);
        pushJsonbValue(&state, WJB_VALUE, &v);
    }

    /* trust_level */
    {
        JsonbValue k, v;
        k.type = jbvString; k.val.string.val = "trust_level";
        k.val.string.len = strlen("trust_level");
        pushJsonbValue(&state, WJB_KEY, &k);

        v.type = jbvString;
        v.val.string.val = sage_trust_level_str ? pstrdup(sage_trust_level_str)
                                                 : pstrdup("observation");
        v.val.string.len = strlen(v.val.string.val);
        pushJsonbValue(&state, WJB_VALUE, &v);
    }

    /* collector_running */
    {
        JsonbValue k, v;
        k.type = jbvString; k.val.string.val = "collector_running";
        k.val.string.len = strlen("collector_running");
        pushJsonbValue(&state, WJB_KEY, &k);

        v.type = jbvBool; v.val.boolean = local_collector_running;
        pushJsonbValue(&state, WJB_VALUE, &v);
    }

    /* analyzer_running */
    {
        JsonbValue k, v;
        k.type = jbvString; k.val.string.val = "analyzer_running";
        k.val.string.len = strlen("analyzer_running");
        pushJsonbValue(&state, WJB_KEY, &k);

        v.type = jbvBool; v.val.boolean = local_analyzer_running;
        pushJsonbValue(&state, WJB_VALUE, &v);
    }

    /* briefing_running */
    {
        JsonbValue k, v;
        k.type = jbvString; k.val.string.val = "briefing_running";
        k.val.string.len = strlen("briefing_running");
        pushJsonbValue(&state, WJB_KEY, &k);

        v.type = jbvBool; v.val.boolean = local_briefing_running;
        pushJsonbValue(&state, WJB_VALUE, &v);
    }

    /* emergency_stopped */
    {
        JsonbValue k, v;
        k.type = jbvString; k.val.string.val = "emergency_stopped";
        k.val.string.len = strlen("emergency_stopped");
        pushJsonbValue(&state, WJB_KEY, &k);

        v.type = jbvBool; v.val.boolean = local_emergency_stopped;
        pushJsonbValue(&state, WJB_VALUE, &v);
    }

    /* last_collect_time */
    {
        JsonbValue k, v;
        k.type = jbvString; k.val.string.val = "last_collect_time";
        k.val.string.len = strlen("last_collect_time");
        pushJsonbValue(&state, WJB_KEY, &k);

        if (local_last_collect_time != 0)
        {
            char *ts = timestamptz_to_str(local_last_collect_time);
            v.type = jbvString; v.val.string.val = ts;
            v.val.string.len = strlen(ts);
        }
        else
        {
            v.type = jbvNull;
        }
        pushJsonbValue(&state, WJB_VALUE, &v);
    }

    /* last_analyze_time */
    {
        JsonbValue k, v;
        k.type = jbvString; k.val.string.val = "last_analyze_time";
        k.val.string.len = strlen("last_analyze_time");
        pushJsonbValue(&state, WJB_KEY, &k);

        if (local_last_analyze_time != 0)
        {
            char *ts = timestamptz_to_str(local_last_analyze_time);
            v.type = jbvString; v.val.string.val = ts;
            v.val.string.len = strlen(ts);
        }
        else
        {
            v.type = jbvNull;
        }
        pushJsonbValue(&state, WJB_VALUE, &v);
    }

    /* last_briefing_time */
    {
        JsonbValue k, v;
        k.type = jbvString; k.val.string.val = "last_briefing_time";
        k.val.string.len = strlen("last_briefing_time");
        pushJsonbValue(&state, WJB_KEY, &k);

        if (local_last_briefing_time != 0)
        {
            char *ts = timestamptz_to_str(local_last_briefing_time);
            v.type = jbvString; v.val.string.val = ts;
            v.val.string.len = strlen(ts);
        }
        else
        {
            v.type = jbvNull;
        }
        pushJsonbValue(&state, WJB_VALUE, &v);
    }

    result = pushJsonbValue(&state, WJB_END_OBJECT, NULL);
    jb = JsonbValueToJsonb(result);

    PG_RETURN_JSONB_P(jb);
}

/* ----------------------------------------------------------------
 * sage_emergency_stop — halt all autonomous actions
 * ---------------------------------------------------------------- */
Datum
sage_emergency_stop(PG_FUNCTION_ARGS)
{
    if (!sage_state)
        ereport(ERROR,
                (errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
                 errmsg("pg_sage shared memory not initialised")));

    LWLockAcquire(sage_state->lock, LW_EXCLUSIVE);
    sage_state->emergency_stopped = true;
    LWLockRelease(sage_state->lock);

    ereport(LOG,
            (errmsg("pg_sage: emergency stop activated")));

    PG_RETURN_BOOL(true);
}

/* ----------------------------------------------------------------
 * sage_resume — resume after emergency stop
 * ---------------------------------------------------------------- */
Datum
sage_resume(PG_FUNCTION_ARGS)
{
    if (!sage_state)
        ereport(ERROR,
                (errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
                 errmsg("pg_sage shared memory not initialised")));

    LWLockAcquire(sage_state->lock, LW_EXCLUSIVE);
    sage_state->emergency_stopped = false;
    LWLockRelease(sage_state->lock);

    ereport(LOG,
            (errmsg("pg_sage: resumed from emergency stop")));

    PG_RETURN_BOOL(true);
}
