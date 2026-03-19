/*
 * pg_sage.h — The Autonomous PostgreSQL DBA Agent
 *
 * Main header for pg_sage extension.
 * AGPL-3.0 License
 */

#ifndef PG_SAGE_H
#define PG_SAGE_H

#include "postgres.h"
#include "fmgr.h"
#include "miscadmin.h"
#include "postmaster/bgworker.h"
#include "storage/ipc.h"
#include "storage/latch.h"
#include "storage/lwlock.h"
#include "storage/proc.h"
#include "storage/shmem.h"
#include "access/xact.h"
#include "executor/spi.h"
#include "utils/builtins.h"
#include "utils/guc.h"
#include "utils/jsonb.h"
#include "utils/snapmgr.h"
#include "utils/timestamp.h"
#include "pgstat.h"
#include "catalog/pg_type.h"
#include "commands/dbcommands.h"
#include "funcapi.h"
#include "utils/memutils.h"

/* ----------------------------------------------------------------
 * Version
 * ---------------------------------------------------------------- */
#define PG_SAGE_VERSION         "0.5.0"
#define PG_SAGE_VERSION_NUM     000500

/* ----------------------------------------------------------------
 * Shared memory state
 * ---------------------------------------------------------------- */

/* Circuit breaker states */
typedef enum SageCircuitState
{
    SAGE_CIRCUIT_CLOSED = 0,    /* Normal operation */
    SAGE_CIRCUIT_OPEN,          /* Tripped — backing off */
    SAGE_CIRCUIT_DORMANT        /* Extended backoff */
} SageCircuitState;

/* Trust levels */
typedef enum SageTrustLevel
{
    SAGE_TRUST_OBSERVATION = 0,
    SAGE_TRUST_ADVISORY,
    SAGE_TRUST_AUTONOMOUS
} SageTrustLevel;

/* Shared state in shmem */
typedef struct SageSharedState
{
    LWLock     *lock;

    /* Circuit breaker */
    SageCircuitState circuit_state;
    int         consecutive_skips;       /* Consecutive skipped cycles */
    int         consecutive_successes;   /* Consecutive successful cycles */
    TimestampTz last_circuit_change;

    /* LLM circuit breaker */
    SageCircuitState llm_circuit_state;
    int         llm_consecutive_failures;
    TimestampTz llm_circuit_opened_at;

    /* Worker coordination */
    bool        collector_running;
    bool        analyzer_running;
    bool        briefing_running;
    TimestampTz last_collect_time;
    TimestampTz last_analyze_time;
    TimestampTz last_briefing_time;

    /* Self-monitoring */
    double      last_collector_duration_ms;
    double      last_analyzer_duration_ms;
    int64       sage_schema_bytes;

    /* HA state */
    bool        is_in_recovery;
    int         role_flip_count;         /* Consecutive role changes */

    /* Emergency stop */
    bool        emergency_stopped;

    /* Trust ramp */
    TimestampTz trust_ramp_start;

    /* Token budget tracking (daily) */
    int         llm_tokens_used_today;
    int         llm_day_of_year;         /* To reset daily counter */

    /* Workload-adaptive scheduling */
    int64       last_xact_commit;        /* Previous cycle's xact_commit */
    TimestampTz last_xact_sample_time;   /* When last_xact_commit was sampled */
    double      xact_rate_per_sec;       /* Computed transaction rate */
    double      baseline_xact_rate;      /* Auto-calibrated baseline */
    int         adaptive_interval_ms;    /* Computed interval in ms */
    int         baseline_samples;        /* Number of samples for calibration */
} SageSharedState;

/* ----------------------------------------------------------------
 * GUC variables (declared in guc.c, extern here)
 * ---------------------------------------------------------------- */
extern bool        sage_enabled;
extern char       *sage_database;
extern int         sage_collector_interval;
extern int         sage_analyzer_interval;
extern int         sage_collector_batch_size;
extern int         sage_slow_query_threshold;
extern int         sage_seq_scan_min_rows;
extern char       *sage_unused_index_window;
extern int         sage_index_bloat_threshold;
extern int         sage_idle_session_timeout;
extern int         sage_disk_pressure_threshold;
extern int         sage_max_connections;
extern char       *sage_trust_level_str;
extern char       *sage_maintenance_window;
extern int         sage_rollback_threshold;
extern int         sage_rollback_window;
extern int         sage_rollback_cooldown;
extern char       *sage_briefing_schedule;
extern char       *sage_briefing_channels;
extern char       *sage_slack_webhook_url;
extern char       *sage_email_smtp_url;
extern bool        sage_llm_enabled;
extern char       *sage_llm_endpoint;
extern char       *sage_llm_api_key;
extern char       *sage_llm_model;
extern int         sage_llm_timeout;
extern int         sage_llm_token_budget;
extern int         sage_llm_context_budget;
extern char       *sage_llm_features;
extern int         sage_llm_cooldown;
extern bool        sage_redact_queries;
extern bool        sage_anonymize_schema;
extern int         sage_react_max_steps;
extern char       *sage_cloud_provider;
extern char       *sage_instance_type;
extern int         sage_retention_snapshots;
extern int         sage_retention_findings;
extern int         sage_retention_actions;
extern int         sage_retention_explains;
extern char       *sage_max_schema_size;
extern double      sage_autoexplain_sample_rate;
extern int         sage_autoexplain_capture_window;

/* ----------------------------------------------------------------
 * Shared memory access
 * ---------------------------------------------------------------- */
extern SageSharedState *sage_state;

/* shmem hooks */
extern void sage_shmem_request(void);
extern void sage_shmem_startup(void);

/* ----------------------------------------------------------------
 * Module init functions
 * ---------------------------------------------------------------- */
extern void sage_guc_init(void);

/* ----------------------------------------------------------------
 * Background worker entry points
 * ---------------------------------------------------------------- */
extern PGDLLEXPORT void sage_collector_main(Datum main_arg);
extern PGDLLEXPORT void sage_analyzer_main(Datum main_arg);
extern PGDLLEXPORT void sage_briefing_main(Datum main_arg);

/* ----------------------------------------------------------------
 * Circuit breaker
 * ---------------------------------------------------------------- */
extern bool sage_circuit_check(void);
extern void sage_circuit_record_success(void);
extern void sage_circuit_record_skip(void);
extern bool sage_llm_circuit_check(void);
extern void sage_llm_circuit_record_failure(void);
extern void sage_llm_circuit_record_success(void);

/* ----------------------------------------------------------------
 * HA awareness
 * ---------------------------------------------------------------- */
extern bool sage_check_recovery_mode(void);
extern bool sage_is_safe_for_writes(void);

/* ----------------------------------------------------------------
 * Trust ramp
 * ---------------------------------------------------------------- */
extern SageTrustLevel sage_get_trust_level(void);
extern int sage_get_trust_day(void);

/* ----------------------------------------------------------------
 * Self-monitoring
 * ---------------------------------------------------------------- */
extern void sage_self_monitor_collect(void);

/* ----------------------------------------------------------------
 * Collector functions
 * ---------------------------------------------------------------- */
extern void sage_collect_stat_statements(void);
extern void sage_collect_table_stats(void);
extern void sage_collect_index_stats(void);
extern void sage_collect_system_stats(void);
extern void sage_collect_lock_stats(void);
extern void sage_collect_sequence_stats(void);
extern void sage_collect_replication_stats(void);

/* ----------------------------------------------------------------
 * Analyzer / rules engine
 * ---------------------------------------------------------------- */
extern void sage_analyze_unused_indexes(void);
extern void sage_analyze_duplicate_indexes(void);
extern void sage_analyze_missing_indexes(void);
extern void sage_analyze_slow_queries(void);
extern void sage_analyze_query_regressions(void);
extern void sage_analyze_seq_scans(void);
extern void sage_analyze_sequence_exhaustion(void);
extern void sage_analyze_config(void);
extern void sage_analyze_index_bloat(void);
extern void sage_analyze_index_write_penalty(void);
extern void sage_analyze_ha_state(void);
extern void sage_run_retention_cleanup(void);
extern void sage_analyze_vacuum_bloat(void);
extern void sage_analyze_security(void);
extern void sage_analyze_replication_health(void);

/* Tier 2 extra analyses */
extern void sage_analyze_cost_attribution(void);
extern void sage_analyze_migration_review(void);
extern void sage_analyze_schema_design(void);

/* ----------------------------------------------------------------
 * Action executor (Tier 3)
 * ---------------------------------------------------------------- */
extern void sage_action_executor_run(void);
extern bool sage_check_maintenance_window(void);
extern void sage_rollback_check(void);

/* ----------------------------------------------------------------
 * Findings helpers
 * ---------------------------------------------------------------- */
extern void sage_upsert_finding(const char *category, const char *severity,
                                const char *object_type, const char *object_id,
                                const char *title, const char *detail_json,
                                const char *recommendation,
                                const char *recommended_sql,
                                const char *rollback_sql);
extern void sage_resolve_finding(const char *category, const char *object_id);

/* ----------------------------------------------------------------
 * LLM interface
 * ---------------------------------------------------------------- */
extern char *sage_llm_call(const char *system_prompt, const char *user_prompt,
                           int max_tokens, int *tokens_used);
extern bool sage_llm_available(void);

/* ----------------------------------------------------------------
 * Context assembly
 * ---------------------------------------------------------------- */
extern char *sage_assemble_context_named(const char *object_name);
extern char *sage_assemble_context_system(void);

/* ----------------------------------------------------------------
 * EXPLAIN capture
 * ---------------------------------------------------------------- */
extern void sage_explain_capture(int64 queryid);
extern char *sage_explain_narrate(int64 queryid);

/* ----------------------------------------------------------------
 * Briefing
 * ---------------------------------------------------------------- */
extern char *sage_generate_briefing(bool use_llm);
extern void sage_deliver_briefing(const char *content, const char *content_json);

/* ----------------------------------------------------------------
 * SQL-callable functions
 * ---------------------------------------------------------------- */
extern Datum sage_status(PG_FUNCTION_ARGS);
extern Datum sage_emergency_stop(PG_FUNCTION_ARGS);
extern Datum sage_resume(PG_FUNCTION_ARGS);
extern Datum sage_explain(PG_FUNCTION_ARGS);
extern Datum sage_suppress(PG_FUNCTION_ARGS);
extern Datum sage_diagnose(PG_FUNCTION_ARGS);
extern Datum sage_briefing(PG_FUNCTION_ARGS);

/* MCP sidecar helper functions (mcp_helpers.c) */
extern Datum sage_health_json(PG_FUNCTION_ARGS);
extern Datum sage_findings_json(PG_FUNCTION_ARGS);
extern Datum sage_schema_json(PG_FUNCTION_ARGS);
extern Datum sage_stats_json(PG_FUNCTION_ARGS);
extern Datum sage_slow_queries_json(PG_FUNCTION_ARGS);
extern Datum sage_explain_json(PG_FUNCTION_ARGS);

/* ----------------------------------------------------------------
 * Utility
 * ---------------------------------------------------------------- */
extern int sage_spi_exec(const char *sql, int expected);
extern char *sage_spi_getval_str(int row, int col);
extern int64 sage_spi_getval_int64(int row, int col);
extern double sage_spi_getval_float(int row, int col);
extern bool sage_spi_isnull(int row, int col);
extern int sage_parse_interval_days(const char *interval_str);
extern TimestampTz sage_now(void);
extern char *sage_escape_json_string(const char *str);
extern char *sage_format_jsonb_object(const char *first_key, ...);
extern void sage_check_suppressions(void);

/* Advisory lock hash for instance dedup */
#define SAGE_ADVISORY_LOCK_HASH     hashtext_simple("pg_sage")

/* Helper for hashtext since it's not always exposed */
static inline int32
hashtext_simple(const char *key)
{
    uint32 h = 0;
    while (*key)
    {
        h = h * 31 + (unsigned char)*key;
        key++;
    }
    return (int32) h;
}

#endif /* PG_SAGE_H */
