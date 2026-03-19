/*
 * guc.c — GUC (Grand Unified Configuration) variables for pg_sage
 *
 * Defines every sage.* setting and registers them in sage_guc_init().
 *
 * AGPL-3.0 License
 */

#include "pg_sage.h"
#include <limits.h>

/* ----------------------------------------------------------------
 * GUC variable definitions
 * ---------------------------------------------------------------- */

/* Core */
bool        sage_enabled                  = true;
char       *sage_database                 = NULL;

/* Collector */
int         sage_collector_interval       = 60;
int         sage_analyzer_interval        = 600;
int         sage_collector_batch_size     = 1000;

/* Thresholds */
int         sage_slow_query_threshold     = 1000;
int         sage_seq_scan_min_rows        = 100000;
char       *sage_unused_index_window      = NULL;
int         sage_index_bloat_threshold    = 30;
int         sage_idle_session_timeout     = 30;
int         sage_disk_pressure_threshold  = 5;

/* Connections */
int         sage_max_connections          = 2;

/* Trust & maintenance */
char       *sage_trust_level_str          = NULL;
char       *sage_maintenance_window       = NULL;

/* Rollback */
int         sage_rollback_threshold       = 10;
int         sage_rollback_window          = 15;
int         sage_rollback_cooldown        = 7;

/* Briefing */
char       *sage_briefing_schedule        = NULL;
char       *sage_briefing_channels        = NULL;
char       *sage_slack_webhook_url        = NULL;
char       *sage_email_smtp_url           = NULL;

/* LLM */
bool        sage_llm_enabled              = false;
char       *sage_llm_endpoint             = NULL;
char       *sage_llm_api_key              = NULL;
char       *sage_llm_model                = NULL;
int         sage_llm_timeout              = 30;
int         sage_llm_token_budget         = 50000;
int         sage_llm_context_budget       = 4096;
char       *sage_llm_features             = NULL;
int         sage_llm_cooldown             = 300;

/* Privacy */
bool        sage_redact_queries           = false;
bool        sage_anonymize_schema         = false;

/* ReAct */
int         sage_react_max_steps          = 10;

/* Cloud */
char       *sage_cloud_provider           = NULL;
char       *sage_instance_type            = NULL;

/* Retention (days) */
int         sage_retention_snapshots      = 90;
int         sage_retention_findings       = 180;
int         sage_retention_actions        = 365;
int         sage_retention_explains       = 90;

/* Self-monitoring */
char       *sage_max_schema_size          = NULL;

/* Auto-explain passive capture */
bool        sage_autoexplain_enabled       = false;
int         sage_autoexplain_min_duration_ms = 1000;
double      sage_autoexplain_sample_rate   = 0.1;
int         sage_autoexplain_capture_window = 300;

/* ----------------------------------------------------------------
 * sage_guc_init — register all sage.* GUC variables
 * ---------------------------------------------------------------- */
void
sage_guc_init(void)
{
    /* --- sage.enabled --- */
    DefineCustomBoolVariable(
        "sage.enabled",
        "Enable or disable pg_sage globally.",
        NULL,
        &sage_enabled,
        true,
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.database --- */
    DefineCustomStringVariable(
        "sage.database",
        "Database that pg_sage workers connect to.",
        NULL,
        &sage_database,
        "postgres",
        PGC_POSTMASTER,
        0,
        NULL, NULL, NULL);

    /* --- sage.collector_interval --- */
    DefineCustomIntVariable(
        "sage.collector_interval",
        "Seconds between collector cycles.",
        NULL,
        &sage_collector_interval,
        60,
        10,
        3600,
        PGC_SIGHUP,
        GUC_UNIT_S,
        NULL, NULL, NULL);

    /* --- sage.analyzer_interval --- */
    DefineCustomIntVariable(
        "sage.analyzer_interval",
        "Seconds between analyzer cycles.",
        NULL,
        &sage_analyzer_interval,
        600,
        60,
        86400,
        PGC_SIGHUP,
        GUC_UNIT_S,
        NULL, NULL, NULL);

    /* --- sage.collector_batch_size --- */
    DefineCustomIntVariable(
        "sage.collector_batch_size",
        "Maximum rows to collect per cycle from pg_stat_statements.",
        NULL,
        &sage_collector_batch_size,
        1000,
        100,
        100000,
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.slow_query_threshold --- */
    DefineCustomIntVariable(
        "sage.slow_query_threshold",
        "Minimum mean_exec_time (ms) to flag a query as slow.",
        NULL,
        &sage_slow_query_threshold,
        1000,
        100,
        60000,
        PGC_SIGHUP,
        GUC_UNIT_MS,
        NULL, NULL, NULL);

    /* --- sage.seq_scan_min_rows --- */
    DefineCustomIntVariable(
        "sage.seq_scan_min_rows",
        "Minimum table rows for a sequential scan to be flagged.",
        NULL,
        &sage_seq_scan_min_rows,
        100000,
        1000,
        INT_MAX,
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.unused_index_window --- */
    DefineCustomStringVariable(
        "sage.unused_index_window",
        "Time window after which an unused index is flagged (e.g. '30d').",
        NULL,
        &sage_unused_index_window,
        "30d",
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.index_bloat_threshold --- */
    DefineCustomIntVariable(
        "sage.index_bloat_threshold",
        "Bloat percentage above which an index is flagged.",
        NULL,
        &sage_index_bloat_threshold,
        30,
        5,
        90,
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.idle_session_timeout --- */
    DefineCustomIntVariable(
        "sage.idle_session_timeout",
        "Minutes of idle-in-transaction before flagging a session.",
        NULL,
        &sage_idle_session_timeout,
        30,
        5,
        1440,
        PGC_SIGHUP,
        GUC_UNIT_MIN,
        NULL, NULL, NULL);

    /* --- sage.disk_pressure_threshold --- */
    DefineCustomIntVariable(
        "sage.disk_pressure_threshold",
        "Percentage of free disk space below which a warning is emitted.",
        NULL,
        &sage_disk_pressure_threshold,
        5,
        1,
        50,
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.max_connections --- */
    DefineCustomIntVariable(
        "sage.max_connections",
        "Maximum number of database connections pg_sage may use.",
        NULL,
        &sage_max_connections,
        2,
        1,
        5,
        PGC_POSTMASTER,
        0,
        NULL, NULL, NULL);

    /* --- sage.trust_level --- */
    DefineCustomStringVariable(
        "sage.trust_level",
        "Trust level: observation, advisory, or autonomous.",
        NULL,
        &sage_trust_level_str,
        "observation",
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.maintenance_window --- */
    DefineCustomStringVariable(
        "sage.maintenance_window",
        "Cron-style window during which autonomous actions are allowed.",
        NULL,
        &sage_maintenance_window,
        "",
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.rollback_threshold --- */
    DefineCustomIntVariable(
        "sage.rollback_threshold",
        "Percentage regression that triggers automatic rollback.",
        NULL,
        &sage_rollback_threshold,
        10,
        1,
        100,
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.rollback_window --- */
    DefineCustomIntVariable(
        "sage.rollback_window",
        "Minutes after an action during which rollback is possible.",
        NULL,
        &sage_rollback_window,
        15,
        5,
        60,
        PGC_SIGHUP,
        GUC_UNIT_MIN,
        NULL, NULL, NULL);

    /* --- sage.rollback_cooldown --- */
    DefineCustomIntVariable(
        "sage.rollback_cooldown",
        "Days to wait before retrying a rolled-back action.",
        NULL,
        &sage_rollback_cooldown,
        7,
        1,
        90,
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.briefing_schedule --- */
    DefineCustomStringVariable(
        "sage.briefing_schedule",
        "Cron expression for daily briefing (e.g. '0 6 * * * UTC').",
        NULL,
        &sage_briefing_schedule,
        "0 6 * * * UTC",
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.briefing_channels --- */
    DefineCustomStringVariable(
        "sage.briefing_channels",
        "Comma-separated delivery channels: stdout, slack, email.",
        NULL,
        &sage_briefing_channels,
        "stdout",
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.slack_webhook_url --- */
    DefineCustomStringVariable(
        "sage.slack_webhook_url",
        "Slack incoming-webhook URL for briefing delivery.",
        NULL,
        &sage_slack_webhook_url,
        "",
        PGC_SIGHUP,
        GUC_SUPERUSER_ONLY,
        NULL, NULL, NULL);

    /* --- sage.email_smtp_url --- */
    DefineCustomStringVariable(
        "sage.email_smtp_url",
        "SMTP URL for email briefing delivery.",
        NULL,
        &sage_email_smtp_url,
        "",
        PGC_SIGHUP,
        GUC_SUPERUSER_ONLY,
        NULL, NULL, NULL);

    /* --- sage.llm_enabled --- */
    DefineCustomBoolVariable(
        "sage.llm_enabled",
        "Enable LLM-powered features.",
        NULL,
        &sage_llm_enabled,
        false,
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.llm_endpoint --- */
    DefineCustomStringVariable(
        "sage.llm_endpoint",
        "HTTP endpoint for LLM API calls.",
        NULL,
        &sage_llm_endpoint,
        "",
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.llm_api_key --- */
    DefineCustomStringVariable(
        "sage.llm_api_key",
        "API key for LLM service.",
        NULL,
        &sage_llm_api_key,
        "",
        PGC_SIGHUP,
        GUC_SUPERUSER_ONLY,
        NULL, NULL, NULL);

    /* --- sage.llm_model --- */
    DefineCustomStringVariable(
        "sage.llm_model",
        "Model name to use for LLM calls.",
        NULL,
        &sage_llm_model,
        "",
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.llm_timeout --- */
    DefineCustomIntVariable(
        "sage.llm_timeout",
        "Timeout in seconds for LLM API calls.",
        NULL,
        &sage_llm_timeout,
        30,
        5,
        120,
        PGC_SIGHUP,
        GUC_UNIT_S,
        NULL, NULL, NULL);

    /* --- sage.llm_token_budget --- */
    DefineCustomIntVariable(
        "sage.llm_token_budget",
        "Maximum tokens per day across all LLM calls.",
        NULL,
        &sage_llm_token_budget,
        50000,
        0,
        INT_MAX,
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.llm_context_budget --- */
    DefineCustomIntVariable(
        "sage.llm_context_budget",
        "Maximum tokens for context assembly per LLM call.",
        NULL,
        &sage_llm_context_budget,
        4096,
        512,
        32768,
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.llm_features --- */
    DefineCustomStringVariable(
        "sage.llm_features",
        "Comma-separated list of LLM-powered features to enable.",
        NULL,
        &sage_llm_features,
        "briefing,explain,diagnostic,shell",
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.llm_cooldown --- */
    DefineCustomIntVariable(
        "sage.llm_cooldown",
        "Minimum seconds between LLM calls for the same finding.",
        NULL,
        &sage_llm_cooldown,
        300,
        30,
        3600,
        PGC_SIGHUP,
        GUC_UNIT_S,
        NULL, NULL, NULL);

    /* --- sage.redact_queries --- */
    DefineCustomBoolVariable(
        "sage.redact_queries",
        "Redact literal values from query texts sent to LLM.",
        NULL,
        &sage_redact_queries,
        false,
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.anonymize_schema --- */
    DefineCustomBoolVariable(
        "sage.anonymize_schema",
        "Anonymise table and column names before sending to LLM.",
        NULL,
        &sage_anonymize_schema,
        false,
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.react_max_steps --- */
    DefineCustomIntVariable(
        "sage.react_max_steps",
        "Maximum steps in a ReAct reasoning chain.",
        NULL,
        &sage_react_max_steps,
        10,
        1,
        50,
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.cloud_provider --- */
    DefineCustomStringVariable(
        "sage.cloud_provider",
        "Cloud provider for cost/sizing recommendations (aws, gcp, azure).",
        NULL,
        &sage_cloud_provider,
        "",
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.instance_type --- */
    DefineCustomStringVariable(
        "sage.instance_type",
        "Current cloud instance type for sizing recommendations.",
        NULL,
        &sage_instance_type,
        "",
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.retention_snapshots --- */
    DefineCustomIntVariable(
        "sage.retention_snapshots",
        "Days to retain snapshot data.",
        NULL,
        &sage_retention_snapshots,
        90,
        1,
        3650,
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.retention_findings --- */
    DefineCustomIntVariable(
        "sage.retention_findings",
        "Days to retain resolved findings.",
        NULL,
        &sage_retention_findings,
        180,
        1,
        3650,
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.retention_actions --- */
    DefineCustomIntVariable(
        "sage.retention_actions",
        "Days to retain action log entries.",
        NULL,
        &sage_retention_actions,
        365,
        1,
        3650,
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.retention_explains --- */
    DefineCustomIntVariable(
        "sage.retention_explains",
        "Days to retain EXPLAIN plan captures.",
        NULL,
        &sage_retention_explains,
        90,
        1,
        3650,
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.max_schema_size --- */
    DefineCustomStringVariable(
        "sage.max_schema_size",
        "Maximum size of the sage schema before self-throttling (e.g. '1GB').",
        NULL,
        &sage_max_schema_size,
        "1GB",
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.autoexplain_enabled --- */
    DefineCustomBoolVariable(
        "sage.autoexplain_enabled",
        "Enable passive EXPLAIN plan capture via ExecutorEnd hook.",
        "When enabled, queries exceeding autoexplain_min_duration_ms are "
        "sampled and their queryid is queued for asynchronous EXPLAIN capture "
        "by the collector worker.",
        &sage_autoexplain_enabled,
        false,
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.autoexplain_min_duration_ms --- */
    DefineCustomIntVariable(
        "sage.autoexplain_min_duration_ms",
        "Minimum query duration (ms) before passive EXPLAIN capture.",
        "Only queries running longer than this threshold are candidates "
        "for auto-explain capture. Set to 0 to capture all queries (not recommended).",
        &sage_autoexplain_min_duration_ms,
        1000,
        0,
        INT_MAX,
        PGC_SIGHUP,
        GUC_UNIT_MS,
        NULL, NULL, NULL);

    /* --- sage.autoexplain_sample_rate --- */
    DefineCustomRealVariable(
        "sage.autoexplain_sample_rate",
        "Fraction of slow queries to auto-EXPLAIN (0.0 to 1.0).",
        "Controls the sampling rate for passive EXPLAIN capture. "
        "A value of 0.1 means 10% of qualifying slow queries will be captured.",
        &sage_autoexplain_sample_rate,
        0.1,
        0.0,
        1.0,
        PGC_SIGHUP,
        0,
        NULL, NULL, NULL);

    /* --- sage.autoexplain_capture_window --- */
    DefineCustomIntVariable(
        "sage.autoexplain_capture_window",
        "Seconds after a finding during which EXPLAIN plans are captured.",
        NULL,
        &sage_autoexplain_capture_window,
        300,
        30,
        1800,
        PGC_SIGHUP,
        GUC_UNIT_S,
        NULL, NULL, NULL);
}
