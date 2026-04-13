package config

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration for pg_sage standalone sidecar.
type Config struct {
	Mode string `yaml:"mode" doc:"Operating mode: extension, standalone, or fleet. Standalone connects to one PostgreSQL target; fleet manages many databases from one sidecar."`

	Postgres   PostgresConfig   `yaml:"postgres"`
	Collector  CollectorConfig  `yaml:"collector"`
	Analyzer   AnalyzerConfig   `yaml:"analyzer"`
	Safety     SafetyConfig     `yaml:"safety"`
	Trust      TrustConfig      `yaml:"trust"`
	LLM        LLMConfig        `yaml:"llm"`
	Advisor    AdvisorConfig    `yaml:"advisor"`
	Briefing    BriefingConfig    `yaml:"briefing"`
	Alerting    AlertingConfig   `yaml:"alerting"`
	AutoExplain AutoExplainConfig `yaml:"auto_explain"`
	Forecaster  ForecasterConfig `yaml:"forecaster"`
	Tuner       TunerConfig      `yaml:"tuner"`
	RCA      RCAConfig      `yaml:"rca"`
	Runaway  RunawayConfig  `yaml:"runaway"`
	Explain  ExplainConfig  `yaml:"explain"`
	LogWatch LogWatchConfig `yaml:"logwatch"`
	Retention   RetentionConfig  `yaml:"retention"`
	Prometheus PrometheusConfig `yaml:"prometheus"`
	OAuth      OAuthConfig      `yaml:"oauth"`

	// Fleet mode fields.
	Databases []DatabaseConfig `yaml:"databases"`
	Defaults  DefaultsConfig   `yaml:"defaults"`
	API       APIConfig        `yaml:"api"`

	// Meta database and encryption (--meta-db, --encryption-key).
	MetaDB        string `yaml:"meta_db" doc:"DSN of the metadata database used in fleet mode to persist cross-target state. Blank in standalone mode."`
	EncryptionKey string `yaml:"encryption_key" doc:"Passphrase used to encrypt sensitive fleet-mode fields (per-database passwords). Rotate via the key-rotation runbook." secret:"true"`

	// Legacy env-var fields (extension mode compat)
	APIKey  string `yaml:"-"`
	TLSCert string `yaml:"-"`
	TLSKey  string `yaml:"-"`

	// Runtime (not from config file)
	ConfigPath         string `yaml:"-"`
	PGVersionNum       int    `yaml:"-"` // e.g. 160000 for PG16
	HasWALColumns      bool   `yaml:"-"`
	HasPlanTimeColumns bool   `yaml:"-"`
	CloudEnvironment   string `yaml:"-"` // e.g. "rds", "cloud-sql", "self-managed"
}

type PostgresConfig struct {
	Host           string `yaml:"host" doc:"Hostname or IP of the target PostgreSQL instance. Ignored when database_url is set."`
	Port           int    `yaml:"port" doc:"TCP port of the target PostgreSQL instance. Ignored when database_url is set."`
	User           string `yaml:"user" doc:"PostgreSQL login role used by the sidecar. Requires pg_read_all_stats plus write access to the sage schema."`
	Password       string `yaml:"password" doc:"Password for the sidecar login role. Prefer environment-variable substitution or a secrets manager over committing literal values." secret:"true"`
	Database       string `yaml:"database" doc:"Target database name the sidecar connects to. Ignored when database_url is set."`
	SSLMode        string `yaml:"sslmode" doc:"libpq sslmode string (disable, allow, prefer, require, verify-ca, verify-full). Use verify-full in production."`
	MaxConnections int    `yaml:"max_connections" doc:"Maximum connections the sidecar pgx pool will open to this target. Keep well below max_connections on the server."`
	DatabaseURL    string `yaml:"database_url" doc:"Full libpq connection URL. When set, overrides host/port/user/password/database/sslmode."`
}

type CollectorConfig struct {
	IntervalSeconds int `yaml:"interval_seconds"`
	BatchSize       int `yaml:"batch_size"`
	MaxQueries      int `yaml:"max_queries"`
}

type AnalyzerConfig struct {
	IntervalSeconds              int     `yaml:"interval_seconds" doc:"How often the analyzer runs its rule checks, in seconds. Lower values detect issues faster but add load."`
	SlowQueryThresholdMs         int     `yaml:"slow_query_threshold_ms" doc:"Queries with mean_exec_time above this are considered slow and become candidates for optimization findings."`
	SeqScanMinRows               int     `yaml:"seq_scan_min_rows" doc:"Minimum table row count before a sequential scan is reported as a finding. Below this, a seq scan is usually cheaper than an index lookup."`
	UnusedIndexWindowDays        int     `yaml:"unused_index_window_days" doc:"An index must be unused (idx_scan=0) for at least this many days before the analyzer suggests dropping it. Default 7 days." warning:"Setting this too low can recommend dropping indexes that support infrequent but critical queries (e.g. monthly reports)."`
	IndexBloatThresholdPct       int     `yaml:"index_bloat_threshold_pct" doc:"Minimum bloat percentage (0-100) an index must exhibit before it becomes a REINDEX candidate."`
	TableBloatDeadTuplePct       int     `yaml:"table_bloat_dead_tuple_pct" doc:"Dead-tuple percentage threshold for reporting table bloat. Above this the analyzer emits a vacuum or repack finding."`
	TableBloatMinRows            int     `yaml:"table_bloat_min_rows" doc:"Minimum row count before a table is considered for bloat analysis. Tiny tables are noisy and not worth reporting."`
	IdleInTxTimeoutMinutes       int     `yaml:"idle_in_transaction_timeout_minutes" doc:"Report sessions sitting in 'idle in transaction' longer than this. Long idle-in-tx sessions block vacuum and hold locks."`
	CacheHitRatioWarning         float64 `yaml:"cache_hit_ratio_warning" doc:"Warn when the shared buffers cache hit ratio falls below this fraction (0.0-1.0). Low ratios suggest shared_buffers is under-sized or working set is too large."`
	XIDWraparoundWarning         int64   `yaml:"xid_wraparound_warning" doc:"Transaction age at which the analyzer emits a wraparound warning. Normally ~200 million."`
	XIDWraparoundCritical        int64   `yaml:"xid_wraparound_critical" doc:"Transaction age at which the analyzer escalates to critical severity. Normally ~1.5 billion — past this autovacuum will force emergency wraparound runs." warning:"If this is hit, autovacuum enters aggressive mode and the database may eventually refuse writes."`
	RegressionThresholdPct       int     `yaml:"regression_threshold_pct" doc:"Percent latency regression required before a query is flagged as a regressor. Set low for sensitive detection, higher to reduce noise."`
	RegressionLookbackDays       int     `yaml:"regression_lookback_days" doc:"How many days of pg_stat_statements history the regression detector compares against."`
	CheckpointFreqWarningPerHour int     `yaml:"checkpoint_frequency_warning_per_hour" doc:"Warn when checkpoints occur more than this many times per hour. High frequency indicates max_wal_size is too small."`

	// v0.8.5 Feature 3 — work_mem role-promotion advisor.
	// Threshold: how many hint applications in a 24h window on a single
	// role/query_hash before promoting to a role-level work_mem finding.
	WorkMemPromotionThreshold int `yaml:"work_mem_promotion_threshold" doc:"How many active work_mem hints a single role must accumulate before the analyzer recommends promoting work_mem at the role level via ALTER ROLE. Set 0 to disable the advisor."`

	// v0.9 — Lock chain detection.
	LockChain LockChainConfig `yaml:"lock_chain"`
}

type SafetyConfig struct {
	CPUCeilingPct            int `yaml:"cpu_ceiling_pct" doc:"Skip cycles when database-host CPU exceeds this percent (1-100). Prevents pg_sage from adding load during incidents."`
	QueryTimeoutMs           int `yaml:"query_timeout_ms" doc:"statement_timeout applied to every collector and analyzer query issued by the sidecar. Protects the target from runaway introspection."`
	DDLTimeoutSeconds        int `yaml:"ddl_timeout_seconds" doc:"Timeout applied to DDL actions (CREATE INDEX, ALTER TABLE) executed by the executor. Per-statement ceiling, not per-cycle."`
	DiskPressureThresholdPct int `yaml:"disk_pressure_threshold_pct" doc:"When database data volume usage exceeds this percent, skip write-producing actions. Prevents adding indexes to a near-full volume."`
	BackoffConsecutiveSkips   int `yaml:"backoff_consecutive_skips" doc:"After this many consecutive skipped cycles (e.g. CPU ceiling hit repeatedly), enter dormant mode and slow the cadence until load subsides."`
	DormantIntervalSeconds   int `yaml:"dormant_interval_seconds" doc:"Cycle interval (seconds) used while in dormant mode — typically much larger than the normal interval so the sidecar wakes rarely while the target is stressed."`
	LockTimeoutMs            int `yaml:"lock_timeout_ms" doc:"lock_timeout applied to connections running DDL or ANALYZE. Must be > 0 for ANALYZE actions in v0.8.5 — autovacuum can hold a ShareUpdateExclusiveLock indefinitely." warning:"Zero disables the timeout entirely and is refused by the executor for ANALYZE actions."`
}

type TrustConfig struct {
	Level                string `yaml:"level" doc:"Autonomy tier. observation = log only; advisory = queue actions for approval; autonomous = execute safe/moderate actions directly." warning:"Changing to autonomous enables DDL execution without human review."`
	RampStart            string `yaml:"ramp_start" doc:"RFC3339 timestamp when the trust ramp began. Auto-persisted on first startup if empty. Used to gate newly-supported actions behind a soak period."`
	MaintenanceWindow    string `yaml:"maintenance_window" doc:"Cron-like window (e.g. 'Sun 02:00-06:00') during which heavy maintenance actions like ANALYZE on large tables or REINDEX are permitted."`
	Tier3Safe            bool   `yaml:"tier3_safe" doc:"Enable tier-3 actions classified as safe (e.g. CREATE INDEX CONCURRENTLY on small tables). Requires trust.level=autonomous."`
	Tier3Moderate        bool   `yaml:"tier3_moderate" doc:"Enable tier-3 actions classified as moderate risk (e.g. VACUUM FULL on small tables). Requires trust.level=autonomous." warning:"Moderate actions can briefly lock tables."`
	Tier3HighRisk        bool   `yaml:"tier3_high_risk" doc:"Enable tier-3 actions classified as high risk. Ignored outside fleet mode — standalone always forces this to false." mode:"fleet-only" warning:"High-risk actions can destabilize production — enable only with a reviewed rollback plan."`
	RollbackThresholdPct int    `yaml:"rollback_threshold_pct" doc:"Latency regression percentage (0-100) that triggers automatic rollback of a recently applied action. Lower = more sensitive."`
	RollbackWindowMinutes int   `yaml:"rollback_window_minutes" doc:"How many minutes after applying an action pg_sage watches for regressions before considering it stable."`
	RollbackCooldownDays  int `yaml:"rollback_cooldown_days" doc:"After a rollback, the same action family is blocked for this many days to prevent flap. Default 3 days."`
	CascadeCooldownCycles int `yaml:"cascade_cooldown_cycles" doc:"If N consecutive cycles each propose new actions, throttle to observe impact. Prevents storm-like cascades of small changes."`
}

type LLMConfig struct {
	Enabled             bool               `yaml:"enabled" doc:"Global enable for LLM-assisted analysis (planner explanations, recommendation narratives). When disabled, pg_sage falls back to deterministic heuristics only."`
	Endpoint            string             `yaml:"endpoint" doc:"LLM provider base URL. Supports OpenAI-compatible chat-completions endpoints. Can be left blank to use the vendor default."`
	APIKey              string             `yaml:"api_key" doc:"Authentication token for the LLM provider. Prefer sourcing from environment via ${LLM_API_KEY} rather than committing literal values." secret:"true"`
	Model               string             `yaml:"model" doc:"Model identifier sent to the provider (e.g. gpt-4o-mini). Must support JSON-mode responses for structured outputs."`
	TimeoutSeconds      int                `yaml:"timeout_seconds" doc:"HTTP request timeout for each LLM call. Low values protect the analyzer cycle from slow providers; high values tolerate cold-start latency."`
	TokenBudgetDaily    int                `yaml:"token_budget_daily" doc:"Soft daily cap on total tokens (input + output) the sidecar will spend on LLM requests. Once exceeded the LLM is skipped until the next UTC day."`
	ContextBudgetTokens int                `yaml:"context_budget_tokens" doc:"Maximum tokens attached as context (schema, stats, plans) to a single LLM request. Prevents oversized prompts from busting the model context window."`
	CooldownSeconds     int                `yaml:"cooldown_seconds" doc:"Minimum seconds between two LLM requests. Rate-limits the sidecar so it cannot burst the provider during a busy cycle."`
	IndexOptimizer      IndexOptimizerConfig `yaml:"index_optimizer"` // Deprecated: use Optimizer.
	Optimizer    OptimizerConfig    `yaml:"optimizer"`
	OptimizerLLM OptimizerLLMConfig `yaml:"optimizer_llm"`
}

type IndexOptimizerConfig struct {
	Enabled           bool `yaml:"enabled"`
	MinQueryCalls     int  `yaml:"min_query_calls"`
	MaxIndexesPerTable int `yaml:"max_indexes_per_table"`
	MaxIncludeColumns int  `yaml:"max_include_columns"`
	OverIndexedRatio  int  `yaml:"over_indexed_ratio_pct"`
	WriteHeavyRatio   int  `yaml:"write_heavy_ratio_pct"`
}

// OptimizerConfig controls the index optimizer v2 behavior.
type OptimizerConfig struct {
	Enabled              bool    `yaml:"enabled"`
	MinQueryCalls        int     `yaml:"min_query_calls"`
	MaxIndexesPerTable   int     `yaml:"max_indexes_per_table"`
	MaxNewPerTable       int     `yaml:"max_new_per_table"`
	MaxIncludeColumns    int     `yaml:"max_include_columns"`
	OverIndexedRatioPct  int     `yaml:"over_indexed_ratio_pct"`
	WriteHeavyRatioPct   int     `yaml:"write_heavy_ratio_pct"`
	MinSnapshots         int     `yaml:"min_snapshots"`
	HypoPGMinImprovePct  float64 `yaml:"hypopg_min_improvement_pct"`
	PlanSource           string  `yaml:"plan_source"`
	ConfidenceThreshold  float64 `yaml:"confidence_threshold"`
	WriteImpactThreshPct float64 `yaml:"write_impact_threshold_pct"`
}

// OptimizerLLMConfig configures the dedicated optimizer LLM (reasoning-tier).
type OptimizerLLMConfig struct {
	Enabled          bool   `yaml:"enabled" doc:"Enable the dedicated reasoning-tier LLM used by the index optimizer. When false the optimizer falls back to the general llm.* client."`
	Endpoint         string `yaml:"endpoint" doc:"Base URL for the optimizer LLM provider. Can differ from the general llm.endpoint when the reasoning tier lives on another provider."`
	APIKey           string `yaml:"api_key" doc:"API key for the optimizer LLM. Prefer env-var substitution over literal values." secret:"true"`
	Model            string `yaml:"model" doc:"Reasoning-tier model identifier (e.g. o1-mini). Must return JSON when requested."`
	TimeoutSeconds   int    `yaml:"timeout_seconds" doc:"Per-request timeout for optimizer LLM calls. Reasoning models typically need longer timeouts than chat models."`
	TokenBudgetDaily int    `yaml:"token_budget_daily" doc:"Daily token cap for the optimizer LLM — independent of the general llm.token_budget_daily so reasoning spend can be tracked separately."`
	CooldownSeconds  int    `yaml:"cooldown_seconds" doc:"Minimum seconds between two optimizer LLM requests."`
	MaxOutputTokens  int    `yaml:"max_output_tokens" doc:"Upper bound on output tokens accepted from the optimizer LLM. Truncates runaway responses."`
	FallbackToGeneral bool  `yaml:"fallback_to_general" doc:"If true, fall back to the general llm.* client when the optimizer LLM is unavailable or over budget."`
}

type AdvisorConfig struct {
	Enabled           bool `yaml:"enabled"`
	IntervalSeconds   int  `yaml:"interval_seconds"`
	VacuumEnabled     bool `yaml:"vacuum_enabled"`
	WALEnabled        bool `yaml:"wal_enabled"`
	ConnectionEnabled bool `yaml:"connection_enabled"`
	MemoryEnabled     bool `yaml:"memory_enabled"`
	RewriteEnabled    bool `yaml:"rewrite_enabled"`
	BloatEnabled      bool `yaml:"bloat_enabled"`
}

type BriefingConfig struct {
	Schedule        string   `yaml:"schedule"`
	Channels        []string `yaml:"channels"`
	SlackWebhookURL string   `yaml:"slack_webhook_url"`
}

type AlertingConfig struct {
	Enabled              bool            `yaml:"enabled"`
	CheckIntervalSeconds int             `yaml:"check_interval_seconds"`
	CooldownMinutes      int             `yaml:"cooldown_minutes"`
	QuietHoursStart      string          `yaml:"quiet_hours_start"`
	QuietHoursEnd        string          `yaml:"quiet_hours_end"`
	Timezone             string          `yaml:"timezone"`
	SlackWebhookURL      string          `yaml:"slack_webhook_url"`
	PagerDutyRoutingKey  string          `yaml:"pagerduty_routing_key"`
	Routes               []AlertRoute    `yaml:"routes"`
	Webhooks             []WebhookConfig `yaml:"webhooks"`
}

type AlertRoute struct {
	Severity string   `yaml:"severity"`
	Channels []string `yaml:"channels"`
}

type WebhookConfig struct {
	Name    string            `yaml:"name"`
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`
}

type AutoExplainConfig struct {
	Enabled                bool `yaml:"enabled"`
	LogMinDurationMs       int  `yaml:"log_min_duration_ms"`
	CollectIntervalSeconds int  `yaml:"collect_interval_seconds"`
	MaxPlansPerCycle       int  `yaml:"max_plans_per_cycle"`
	PreferSessionLoad      bool `yaml:"prefer_session_load"`
}

type ForecasterConfig struct {
	Enabled              bool    `yaml:"enabled"`
	LookbackDays         int     `yaml:"lookback_days"`
	DiskWarnGrowthGBDay  float64 `yaml:"disk_warn_growth_gb_day"`
	ConnectionWarnPct    float64 `yaml:"connection_warn_pct"`
	CacheWarnThreshold   float64 `yaml:"cache_warn_threshold"`
	SequenceWarnDays     int     `yaml:"sequence_warn_days"`
	SequenceCriticalDays int     `yaml:"sequence_critical_days"`

	// v0.9 fields
	MinDataPoints     int     `yaml:"min_data_points" doc:"Minimum data points required before generating a forecast. Default: 24."`
	AlertHorizons     []int   `yaml:"alert_horizons" doc:"Days-until-full thresholds that generate findings. Default: [30, 7, 3]."`
	DiskCapacityBytes int64   `yaml:"disk_capacity_bytes" doc:"Total disk capacity in bytes. 0 = auto-detect. Set explicitly for managed services (RDS, Cloud SQL)."`
	MinRSquared       float64 `yaml:"min_r_squared" doc:"Minimum R-squared for forecast reliability. Below this, findings are info-only. Default: 0.5."`
}

// RCAConfig controls the root cause analysis engine (v0.9).
type RCAConfig struct {
	Enabled                  bool    `yaml:"enabled" doc:"Enable the root cause analysis engine. Correlates multiple signals into unified incident narratives."`
	LLMCorrelationThreshold  int     `yaml:"llm_correlation_threshold" doc:"Minimum concurrent signals to trigger LLM-based correlation (Tier 2). Default: 3."`
	DedupWindowMinutes       int     `yaml:"dedup_window_minutes" doc:"Time window for deduplicating incidents with the same signals and root object. Default: 30."`
	EscalationCycles         int     `yaml:"escalation_cycles" doc:"Consecutive analyzer cycles an incident stays open before escalating warning to critical. Default: 5."`
	ResolutionCycles         int     `yaml:"resolution_cycles" doc:"Consecutive clear cycles before an incident auto-resolves. Default: 2."`
	ConnectionSaturationPct  int     `yaml:"connection_saturation_pct" doc:"Percentage of max_connections that triggers the connections_high signal. Default: 80."`
	ReplicationLagThresholdS int     `yaml:"replication_lag_threshold_seconds" doc:"Seconds of replay lag before the replication_lag_increasing signal fires. Default: 30."`
	WALSpikeMultiplier       float64 `yaml:"wal_spike_multiplier" doc:"WAL bytes delta must exceed previous delta by this multiplier to trigger wal_growth_spike. Default: 2.0."`
}

// LockChainConfig controls lock chain detection (v0.9).
type LockChainConfig struct {
	Enabled                  bool     `yaml:"enabled" doc:"Enable lock chain detection using pg_blocking_pids(). Detects cascading lock waits."`
	MinBlockedThreshold      int      `yaml:"min_blocked_threshold" doc:"Minimum blocked sessions before a lock chain generates a finding. Default: 3."`
	CriticalBlockedThreshold int      `yaml:"critical_blocked_threshold" doc:"Blocked session count that escalates finding to critical severity. Default: 10."`
	IdleInTxTerminateMinutes int      `yaml:"idle_in_tx_terminate_minutes" doc:"Minutes an idle-in-transaction root blocker must wait before pg_terminate_backend is recommended. Default: 5."`
	ActiveQueryCancelMinutes int      `yaml:"active_query_cancel_minutes" doc:"Minutes an active query root blocker must run before pg_cancel_backend is recommended. Default: 15."`
	SafePatterns             []string `yaml:"safe_patterns" doc:"Application name substrings that protect a process from automated termination/cancellation."`
}

// LogWatchConfig controls PostgreSQL log file monitoring (v0.9.1).
type LogWatchConfig struct {
	Enabled             bool     `yaml:"enabled" doc:"Enable log file monitoring for RCA. Requires log_destination includes jsonlog or csvlog. Requires rca.enabled=true."`
	LogDirectory        string   `yaml:"log_directory" doc:"Path to PG log directory. Auto-detected from pg_settings if blank."`
	Format              string   `yaml:"format" doc:"Log format: jsonlog (PG15+) or csvlog. Auto-detected if blank."`
	PollIntervalMs      int      `yaml:"poll_interval_ms" doc:"Fallback poll interval when fsnotify is unavailable. Default: 1000."`
	DedupWindowS        int      `yaml:"dedup_window_seconds" doc:"Suppress duplicate log signals within this window. Default: 60."`
	MaxLineLenBytes     int      `yaml:"max_line_len_bytes" doc:"Discard log lines longer than this. Default: 65536."`
	TempFileMinBytes    int      `yaml:"temp_file_min_bytes" doc:"Minimum temp file size to trigger log_temp_file_created. Default: 10485760."`
	MaxLinesPerCycle    int      `yaml:"max_lines_per_cycle" doc:"Stop parsing after this many lines per drain cycle. Default: 10000."`
	ExcludeApplications []string `yaml:"exclude_applications" doc:"Application names excluded from timeout/deadlock signals. Default: pg_sage."`
	SlowQueryEnabled    bool     `yaml:"slow_query_enabled" doc:"Opt-in: parse log_min_duration_statement output as log_slow_query signals. Default: false."`
}

// RunawayPolicy defines when a query is considered runaway (v0.9).
type RunawayPolicy struct {
	Name               string `yaml:"name"`
	MaxDurationMinutes int    `yaml:"max_duration_minutes"`
	MaxBlockedSessions int    `yaml:"max_blocked_sessions"`
	WarnCycles         int    `yaml:"warn_cycles"`
	CancelCycles       int    `yaml:"cancel_cycles"`
}

// RunawayConfig controls runaway query termination (v0.9).
type RunawayConfig struct {
	Enabled      bool            `yaml:"enabled" doc:"Enable policy-based runaway query detection and escalation (warn -> cancel -> terminate)."`
	Policies     []RunawayPolicy `yaml:"policies" doc:"List of runaway query policies. A query matching any policy enters the escalation state machine."`
	SafePatterns []string        `yaml:"safe_patterns" doc:"Application name substrings protected from runaway termination. Merged with lock_chain.safe_patterns at startup."`
}

// ExplainConfig controls the natural language EXPLAIN feature (v0.9).
type ExplainConfig struct {
	Enabled         bool `yaml:"enabled" doc:"Enable the LLM-powered natural language EXPLAIN API endpoint."`
	TimeoutMs       int  `yaml:"timeout_ms" doc:"statement_timeout for EXPLAIN ANALYZE execution. Default: 10000."`
	CacheTTLMinutes int  `yaml:"cache_ttl_minutes" doc:"TTL for cached explain results. Default: 60."`
	MaxTokens       int  `yaml:"max_tokens" doc:"Maximum LLM output tokens for explain responses. Default: 4096."`
}

type TunerConfig struct {
	Enabled                bool    `yaml:"enabled" doc:"Master switch for the per-query tuner. When false, no hints are written and Tune() is a no-op — the analyzer still runs."`
	LLMEnabled             bool    `yaml:"llm_enabled" doc:"Allow the tuner to call the LLM for hint prescription when deterministic rules do not produce a recommendation. Budget-gated by llm.token_budget_daily."`
	WorkMemMaxMB           int     `yaml:"work_mem_max_mb" doc:"Maximum per-query work_mem (MB) the tuner is allowed to prescribe via Set(work_mem) hints. Ceiling prevents runaway memory suggestions."`
	PlanTimeRatio          float64 `yaml:"plan_time_ratio" doc:"When plan_time / exec_time exceeds this ratio, the tuner flags the query as plan-time-dominated and considers prepared-statement hints."`
	NestedLoopRowThreshold int64   `yaml:"nested_loop_row_threshold" doc:"Minimum actual-row count on a nested-loop inner that triggers the bad-nested-loop symptom. Smaller values increase sensitivity."`
	ParallelMinTableRows   int64   `yaml:"parallel_min_table_rows" doc:"Threshold for suggesting parallel-enable hints on large sequential scans. Tables smaller than this are assumed not to benefit from parallelism."`
	MinQueryCalls          int     `yaml:"min_query_calls" doc:"Require at least this many pg_stat_statements.calls before the tuner will act on a query. Filters out one-offs."`
	VerifyAfterApply       bool    `yaml:"verify_after_apply" doc:"Run the daily hint revalidation loop. Removes hints that have become redundant, harmful, or broken. Requires pg_hint_plan." warning:"v0.8.5 activates a real background cycle here — upgrade note: if you want v0.8.4 inert behavior, set this to false before upgrading."`

	// v0.8.5 Feature 1 — Hint revalidation loop.
	HintRetirementDays           int     `yaml:"hint_retirement_days" doc:"Hints older than this many days are retired unconditionally, regardless of current query behavior. Safety net against stale hint accumulation."`
	RevalidationIntervalHours    int     `yaml:"revalidation_interval_hours" doc:"How often the hint revalidation loop runs. Set to 0 to disable the loop entirely. Default 24 hours."`
	RevalidationKeepRatio        float64 `yaml:"revalidation_keep_ratio" doc:"During cost comparison, hinted plan cost must be at most this ratio of the unhinted plan to keep the hint. Default 1.2 (hinted <= 120% of unhinted)."`
	RevalidationRollbackRatio    float64 `yaml:"revalidation_rollback_ratio" doc:"When hinted-plan cost exceeds unhinted by this ratio, the hint is marked broken and rolled back. Default 0.8 (hinted >= 125% of unhinted)."`
	RevalidationExplainTimeoutMs int     `yaml:"revalidation_explain_timeout_ms" doc:"statement_timeout applied to EXPLAIN queries issued by the revalidation loop. Queries that cannot be explained in time are deferred."`

	// v0.8.5 Feature 2 — Stale-stats detection + ANALYZE action.
	StaleStatsEstimateSkew        float64 `yaml:"stale_stats_estimate_skew" doc:"Ratio ActualRows / PlanRows above which a plan node is considered row-estimate skewed. Default 10 — same threshold as the bad-nested-loop check."`
	StaleStatsModRatio            float64 `yaml:"stale_stats_mod_ratio" doc:"Fraction of rows modified since last ANALYZE (n_mod_since_analyze / reltuples) required to treat a table as stale. Default 0.1 = 10%."`
	StaleStatsAgeMinutes          int     `yaml:"stale_stats_age_minutes" doc:"Both manual and autovacuum last_analyze must be older than this many minutes before the table is considered stale. Default 60."`
	AnalyzeMaxTableMB             int64   `yaml:"analyze_max_table_mb" doc:"Tables larger than this many MB are never auto-ANALYZEd — an advisory finding is emitted instead for operator review." warning:"Default 10240 (10GB). Raise with care — ANALYZE on TB-scale tables can saturate I/O."`
	AnalyzeCooldownMinutes        int     `yaml:"analyze_cooldown_minutes" doc:"Minimum minutes between tuner-initiated ANALYZE runs on the same table. Also respects autovacuum last_analyze as a recent-run signal."`
	AnalyzeMaintenanceThresholdMB int64   `yaml:"analyze_maintenance_threshold_mb" doc:"Tables larger than this many MB can only be ANALYZEd during the configured maintenance_window. Default 1024 (1GB)."`
	AnalyzeTimeoutMs              int     `yaml:"analyze_timeout_ms" doc:"statement_timeout for ANALYZE actions. Default 600000 (10 minutes). Exceeding this marks the action failed_timeout and extends the cooldown."`
	MaxConcurrentAnalyze          int     `yaml:"max_concurrent_analyze" doc:"Hard cap on concurrent ANALYZE actions across the entire sidecar process (shared semaphore — fleet-wide, not per-database). Default 1."`
}

type RetentionConfig struct {
	SnapshotsDays int `yaml:"snapshots_days"`
	FindingsDays  int `yaml:"findings_days"`
	ActionsDays   int `yaml:"actions_days"`
	ExplainsDays  int `yaml:"explains_days"`
}

type PrometheusConfig struct {
	ListenAddr string `yaml:"listen_addr"`
}

// OAuthConfig holds OAuth2/OIDC SSO settings.
type OAuthConfig struct {
	Enabled      bool   `yaml:"enabled" doc:"Enable OAuth single sign-on for the sidecar UI. When false, the UI falls back to local auth."`
	Provider     string `yaml:"provider" doc:"OAuth provider identifier. Supported values include google, github, okta, and generic OIDC."`
	ClientID     string `yaml:"client_id" doc:"OAuth client ID issued by the provider when the pg_sage application was registered."`
	ClientSecret string `yaml:"client_secret" doc:"OAuth client secret issued by the provider. Source from an environment variable in production." secret:"true"`
	RedirectURL  string `yaml:"redirect_url" doc:"Absolute URL the provider redirects to after authentication. Must match the URI registered at the provider."`
	IssuerURL    string `yaml:"issuer_url" doc:"OIDC issuer URL used to discover the provider's authorization and token endpoints."`
	DefaultRole  string `yaml:"default_role" doc:"Role assigned to newly authenticated users when no role mapping rule matches."`
}

// Interval helpers.
func (c *CollectorConfig) Interval() time.Duration {
	return time.Duration(c.IntervalSeconds) * time.Second
}

func (c *AnalyzerConfig) Interval() time.Duration {
	return time.Duration(c.IntervalSeconds) * time.Second
}

func (c *AdvisorConfig) Interval() time.Duration {
	return time.Duration(c.IntervalSeconds) * time.Second
}

func (c *SafetyConfig) DormantInterval() time.Duration {
	return time.Duration(c.DormantIntervalSeconds) * time.Second
}

func (c *AlertingConfig) CheckInterval() time.Duration {
	return time.Duration(c.CheckIntervalSeconds) * time.Second
}

func (c *AutoExplainConfig) CollectInterval() time.Duration {
	return time.Duration(c.CollectIntervalSeconds) * time.Second
}

func (c *SafetyConfig) QueryTimeout() time.Duration {
	return time.Duration(c.QueryTimeoutMs) * time.Millisecond
}

func (c *SafetyConfig) DDLTimeout() time.Duration {
	return time.Duration(c.DDLTimeoutSeconds) * time.Second
}

func (c *SafetyConfig) LockTimeout() int {
	if c.LockTimeoutMs <= 0 {
		return DefaultLockTimeoutMs
	}
	return c.LockTimeoutMs
}

// DSN builds a libpq connection string from individual fields.
func (p *PostgresConfig) DSN() string {
	if p.DatabaseURL != "" {
		return p.DatabaseURL
	}
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		p.Host, p.Port, p.User, p.Password, p.Database, p.SSLMode,
	)
}

// Load reads config from YAML file, then overlays env vars, then CLI flags.
// Precedence: CLI > env > YAML > defaults.
func Load(args []string) (*Config, error) {
	cfg := newDefaults()

	// Parse CLI flags to get config path and mode early.
	fs := flag.NewFlagSet("pg_sage_sidecar", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to config.yaml")
	mode := fs.String("mode", "", "Operating mode: extension or standalone")
	pgHost := fs.String("pg-host", "", "PostgreSQL host")
	pgPort := fs.Int("pg-port", 0, "PostgreSQL port")
	pgUser := fs.String("pg-user", "", "PostgreSQL user")
	pgPassword := fs.String("pg-password", "", "PostgreSQL password")
	pgDatabase := fs.String("pg-database", "", "PostgreSQL database")
	pgSSLMode := fs.String("pg-sslmode", "", "PostgreSQL sslmode")
	pgURL := fs.String("pg-url", "", "PostgreSQL connection URL (overrides individual pg-* flags)")
	promAddr := fs.String("prom-addr", "", "Prometheus listen address")
	metaDB := fs.String("meta-db", "", "Metadata database connection string")
	encryptionKey := fs.String("encryption-key", "", "Passphrase for credential encryption")
	_ = fs.Parse(args)

	// Step 1: Load YAML (if provided or auto-detected).
	yamlPath := *configPath
	if yamlPath == "" {
		yamlPath = envOr("SAGE_CONFIG_PATH", "")
	}
	if yamlPath == "" {
		// Auto-detect config.yaml in CWD.
		if _, err := os.Stat("config.yaml"); err == nil {
			yamlPath = "config.yaml"
		}
	}
	if yamlPath != "" {
		if err := loadYAML(yamlPath, cfg); err != nil {
			return nil, fmt.Errorf("loading config %s: %w", yamlPath, err)
		}
		cfg.ConfigPath = yamlPath
	}

	// Step 2: Overlay environment variables.
	overlayEnv(cfg)

	// Step 3: Overlay CLI flags (highest precedence).
	if *mode != "" {
		cfg.Mode = *mode
	}
	if *pgHost != "" {
		cfg.Postgres.Host = *pgHost
	}
	if *pgPort != 0 {
		cfg.Postgres.Port = *pgPort
	}
	if *pgUser != "" {
		cfg.Postgres.User = *pgUser
	}
	if *pgPassword != "" {
		cfg.Postgres.Password = *pgPassword
	}
	if *pgDatabase != "" {
		cfg.Postgres.Database = *pgDatabase
	}
	if *pgSSLMode != "" {
		cfg.Postgres.SSLMode = *pgSSLMode
	}
	if *pgURL != "" {
		cfg.Postgres.DatabaseURL = *pgURL
	}
	if *promAddr != "" {
		cfg.Prometheus.ListenAddr = *promAddr
	}
	if *metaDB != "" {
		cfg.MetaDB = *metaDB
	}
	if *encryptionKey != "" {
		cfg.EncryptionKey = *encryptionKey
	}

	// Legacy env-var compat.
	cfg.APIKey = os.Getenv("SAGE_API_KEY")
	cfg.TLSCert = os.Getenv("SAGE_TLS_CERT")
	cfg.TLSKey = os.Getenv("SAGE_TLS_KEY")

	// Normalize fleet/standalone config before validation.
	cfg.normalize()

	// Validate mode.
	if cfg.Mode != "extension" && cfg.Mode != "standalone" && cfg.Mode != "fleet" {
		return nil, fmt.Errorf(
			"invalid mode %q: must be 'extension', 'standalone', or 'fleet'", cfg.Mode)
	}
	if cfg.Mode == "standalone" && cfg.Postgres.DSN() == "" && cfg.MetaDB == "" {
		return nil, fmt.Errorf(
			"standalone mode requires postgres connection config or --meta-db")
	}

	// Migrate deprecated index_optimizer → optimizer if optimizer wasn't
	// explicitly set but the legacy key was.
	if !cfg.LLM.Optimizer.Enabled && cfg.LLM.IndexOptimizer.Enabled {
		cfg.LLM.Optimizer.Enabled = true
		if cfg.LLM.IndexOptimizer.MinQueryCalls > 0 {
			cfg.LLM.Optimizer.MinQueryCalls = cfg.LLM.IndexOptimizer.MinQueryCalls
		}
		if cfg.LLM.IndexOptimizer.MaxIndexesPerTable > 0 {
			cfg.LLM.Optimizer.MaxIndexesPerTable = cfg.LLM.IndexOptimizer.MaxIndexesPerTable
		}
		if cfg.LLM.IndexOptimizer.MaxIncludeColumns > 0 {
			cfg.LLM.Optimizer.MaxIncludeColumns = cfg.LLM.IndexOptimizer.MaxIncludeColumns
		}
		if cfg.LLM.IndexOptimizer.OverIndexedRatio > 0 {
			cfg.LLM.Optimizer.OverIndexedRatioPct = cfg.LLM.IndexOptimizer.OverIndexedRatio
		}
		if cfg.LLM.IndexOptimizer.WriteHeavyRatio > 0 {
			cfg.LLM.Optimizer.WriteHeavyRatioPct = cfg.LLM.IndexOptimizer.WriteHeavyRatio
		}
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}

	return cfg, nil
}

func (c *Config) validate() error {
	if c.Collector.IntervalSeconds <= 0 {
		return fmt.Errorf("collector.interval_seconds must be positive")
	}
	if c.Collector.BatchSize <= 0 {
		return fmt.Errorf("collector.batch_size must be positive")
	}
	if c.Collector.MaxQueries <= 0 {
		return fmt.Errorf("collector.max_queries must be positive")
	}
	if c.Analyzer.IntervalSeconds <= 0 {
		return fmt.Errorf("analyzer.interval_seconds must be positive")
	}
	if c.Analyzer.SlowQueryThresholdMs < 0 {
		return fmt.Errorf("analyzer.slow_query_threshold_ms must be non-negative")
	}
	validTrust := map[string]bool{
		"observation": true, "advisory": true, "autonomous": true,
	}
	if !validTrust[c.Trust.Level] {
		return fmt.Errorf(
			"trust.level must be observation, advisory, or autonomous (got %q)",
			c.Trust.Level,
		)
	}
	if c.Safety.CPUCeilingPct <= 0 || c.Safety.CPUCeilingPct > 100 {
		return fmt.Errorf("safety.cpu_ceiling_pct must be 1-100")
	}
	if c.Safety.QueryTimeoutMs <= 0 {
		return fmt.Errorf("safety.query_timeout_ms must be positive")
	}

	// Fleet-specific validation.
	if c.Mode == "fleet" {
		if len(c.Databases) == 0 && !c.HasMetaDB() {
			return fmt.Errorf("fleet mode requires at least one database")
		}
		seen := make(map[string]bool, len(c.Databases))
		for i, db := range c.Databases {
			if db.Name == "" {
				return fmt.Errorf("databases[%d]: name must not be empty", i)
			}
			if db.Host == "" {
				return fmt.Errorf("databases[%d] %q: host must not be empty", i, db.Name)
			}
			if seen[db.Name] {
				return fmt.Errorf("databases[%d]: duplicate name %q", i, db.Name)
			}
			seen[db.Name] = true
		}
	}

	return nil
}

func newDefaults() *Config {
	return &Config{
		Mode: DefaultMode,
		Postgres: PostgresConfig{
			Host:           "localhost",
			Port:           DefaultPGPort,
			User:           DefaultPGUser,
			Database:       DefaultPGDatabase,
			SSLMode:        DefaultPGSSLMode,
			MaxConnections: DefaultPGMaxConnections,
		},
		Collector: CollectorConfig{
			IntervalSeconds: int(DefaultCollectorInterval / time.Second),
			BatchSize:       DefaultCollectorBatchSize,
			MaxQueries:      DefaultCollectorMaxQueries,
		},
		Analyzer: AnalyzerConfig{
			IntervalSeconds:              int(DefaultAnalyzerInterval / time.Second),
			SlowQueryThresholdMs:         DefaultSlowQueryThresholdMs,
			SeqScanMinRows:               DefaultSeqScanMinRows,
			UnusedIndexWindowDays:        DefaultUnusedIndexWindowDays,
			IndexBloatThresholdPct:       DefaultIndexBloatThresholdPct,
			TableBloatDeadTuplePct:       DefaultTableBloatDeadTuplePct,
			TableBloatMinRows:           DefaultTableBloatMinRows,
			IdleInTxTimeoutMinutes:       DefaultIdleInTxTimeoutMinutes,
			CacheHitRatioWarning:         DefaultCacheHitRatioWarning,
			XIDWraparoundWarning:         DefaultXIDWraparoundWarning,
			XIDWraparoundCritical:        DefaultXIDWraparoundCritical,
			RegressionThresholdPct:       DefaultRegressionThresholdPct,
			RegressionLookbackDays:       DefaultRegressionLookbackDays,
			CheckpointFreqWarningPerHour: DefaultCheckpointFreqWarningPerHour,
			WorkMemPromotionThreshold:    DefaultAnalyzerWorkMemPromotionThreshold,
			LockChain: LockChainConfig{
				Enabled:                  true,
				MinBlockedThreshold:      DefaultLockChainMinBlocked,
				CriticalBlockedThreshold: DefaultLockChainCriticalBlocked,
				IdleInTxTerminateMinutes: DefaultLockChainIdleInTxTerminate,
				ActiveQueryCancelMinutes: DefaultLockChainActiveQueryCancel,
				SafePatterns:             []string{"pg_sage", "replication", "patroni"},
			},
		},
		Safety: SafetyConfig{
			CPUCeilingPct:            DefaultCPUCeilingPct,
			QueryTimeoutMs:           DefaultQueryTimeoutMs,
			DDLTimeoutSeconds:        DefaultDDLTimeoutSeconds,
			DiskPressureThresholdPct: DefaultDiskPressureThresholdPct,
			BackoffConsecutiveSkips:  DefaultBackoffConsecutiveSkips,
			DormantIntervalSeconds:   DefaultDormantIntervalSeconds,
			LockTimeoutMs:            DefaultLockTimeoutMs,
		},
		Trust: TrustConfig{
			Level:                 DefaultTrustLevel,
			Tier3Safe:             DefaultTier3Safe,
			Tier3Moderate:         DefaultTier3Moderate,
			Tier3HighRisk:         DefaultTier3HighRisk,
			RollbackThresholdPct:  DefaultRollbackThresholdPct,
			RollbackWindowMinutes: DefaultRollbackWindowMinutes,
			RollbackCooldownDays:  DefaultRollbackCooldownDays,
			CascadeCooldownCycles: DefaultCascadeCooldownCycles,
		},
		LLM: LLMConfig{
			Enabled:            DefaultLLMEnabled,
			TimeoutSeconds:     DefaultLLMTimeoutSeconds,
			TokenBudgetDaily:   DefaultLLMTokenBudget,
			ContextBudgetTokens: DefaultLLMContextBudget,
			CooldownSeconds:    DefaultLLMCooldownSeconds,
			IndexOptimizer: IndexOptimizerConfig{
				Enabled:            DefaultIdxOptEnabled,
				MinQueryCalls:      DefaultIdxOptMinQueryCalls,
				MaxIndexesPerTable: DefaultIdxOptMaxIndexesPerTable,
				MaxIncludeColumns:  DefaultIdxOptMaxIncludeColumns,
				OverIndexedRatio:   DefaultIdxOptOverIndexedRatio,
				WriteHeavyRatio:    DefaultIdxOptWriteHeavyRatio,
			},
			Optimizer: OptimizerConfig{
				Enabled:              DefaultOptEnabled,
				MinQueryCalls:        DefaultOptMinQueryCalls,
				MaxIndexesPerTable:   DefaultOptMaxIndexesPerTable,
				MaxNewPerTable:       DefaultOptMaxNewPerTable,
				MaxIncludeColumns:    DefaultOptMaxIncludeColumns,
				OverIndexedRatioPct:  DefaultOptOverIndexedRatioPct,
				WriteHeavyRatioPct:   DefaultOptWriteHeavyRatioPct,
				MinSnapshots:         DefaultOptMinSnapshots,
				HypoPGMinImprovePct:  DefaultOptHypoPGMinImprovePct,
				PlanSource:           DefaultOptPlanSource,
				ConfidenceThreshold:  DefaultOptConfidenceThreshold,
				WriteImpactThreshPct: DefaultOptWriteImpactThreshPct,
			},
			OptimizerLLM: OptimizerLLMConfig{
				Enabled:           false,
				TimeoutSeconds:    DefaultOptLLMTimeoutSeconds,
				TokenBudgetDaily:  DefaultOptLLMTokenBudget,
				CooldownSeconds:   DefaultOptLLMCooldownSeconds,
				MaxOutputTokens:   DefaultOptLLMMaxOutputTokens,
				FallbackToGeneral: true,
			},
		},
		Advisor: AdvisorConfig{
			Enabled:           false,
			IntervalSeconds:   86400,
			VacuumEnabled:     true,
			WALEnabled:        true,
			ConnectionEnabled: true,
			MemoryEnabled:     true,
			RewriteEnabled:    true,
			BloatEnabled:      true,
		},
		Briefing: BriefingConfig{
			Schedule: DefaultBriefingSchedule,
			Channels: []string{"stdout"},
		},
		Alerting: AlertingConfig{
			Enabled:              false,
			CheckIntervalSeconds: DefaultAlertingCheckInterval,
			CooldownMinutes:      DefaultAlertingCooldown,
			Timezone:             "UTC",
		},
		AutoExplain: AutoExplainConfig{
			Enabled:                true,
			LogMinDurationMs:       DefaultAutoExplainLogMinDuration,
			CollectIntervalSeconds: DefaultAutoExplainCollectInterval,
			MaxPlansPerCycle:       DefaultAutoExplainMaxPlansPerCycle,
			PreferSessionLoad:      true,
		},
		Forecaster: ForecasterConfig{
			Enabled:              true,
			LookbackDays:         DefaultForecasterLookbackDays,
			DiskWarnGrowthGBDay:  DefaultForecasterDiskWarnGBDay,
			ConnectionWarnPct:    DefaultForecasterConnectionPct,
			CacheWarnThreshold:   DefaultForecasterCacheThreshold,
			SequenceWarnDays:     DefaultForecasterSeqWarnDays,
			SequenceCriticalDays: DefaultForecasterSeqCritDays,
			MinDataPoints:     DefaultForecasterMinDataPoints,
			AlertHorizons:     []int{30, 7, 3},
			DiskCapacityBytes: 0,
			MinRSquared:       DefaultForecasterMinRSquared,
		},
		Tuner: TunerConfig{
			Enabled:                true,
			WorkMemMaxMB:           DefaultTunerWorkMemMaxMB,
			PlanTimeRatio:          DefaultTunerPlanTimeRatio,
			NestedLoopRowThreshold: DefaultTunerNestedLoopRowThresh,
			ParallelMinTableRows:   DefaultTunerParallelMinRows,
			MinQueryCalls:          DefaultTunerMinQueryCalls,
			VerifyAfterApply:       true,

			// Feature 1 — Hint revalidation loop.
			HintRetirementDays:           DefaultTunerHintRetirementDays,
			RevalidationIntervalHours:    DefaultTunerRevalidationIntervalHours,
			RevalidationKeepRatio:        DefaultTunerRevalidationKeepRatio,
			RevalidationRollbackRatio:    DefaultTunerRevalidationRollbackRatio,
			RevalidationExplainTimeoutMs: DefaultTunerRevalidationExplainTimeoutMs,

			// Feature 2 — Stale-stats detection + ANALYZE.
			StaleStatsEstimateSkew:        DefaultTunerStaleStatsEstimateSkew,
			StaleStatsModRatio:            DefaultTunerStaleStatsModRatio,
			StaleStatsAgeMinutes:          DefaultTunerStaleStatsAgeMinutes,
			AnalyzeMaxTableMB:             DefaultTunerAnalyzeMaxTableMB,
			AnalyzeCooldownMinutes:        DefaultTunerAnalyzeCooldownMinutes,
			AnalyzeMaintenanceThresholdMB: DefaultTunerAnalyzeMaintenanceThresholdMB,
			AnalyzeTimeoutMs:              DefaultTunerAnalyzeTimeoutMs,
			MaxConcurrentAnalyze:          DefaultTunerMaxConcurrentAnalyze,
		},
		RCA: RCAConfig{
			Enabled:                  true,
			LLMCorrelationThreshold:  DefaultRCALLMCorrelationThreshold,
			DedupWindowMinutes:       DefaultRCADedupWindowMinutes,
			EscalationCycles:         DefaultRCAEscalationCycles,
			ResolutionCycles:         DefaultRCAResolutionCycles,
			ConnectionSaturationPct:  DefaultRCAConnectionSaturationPct,
			ReplicationLagThresholdS: DefaultRCAReplicationLagThresholdS,
			WALSpikeMultiplier:       DefaultRCAWALSpikeMultiplier,
		},
		Runaway: RunawayConfig{
			Enabled: false,
			Policies: []RunawayPolicy{
				{Name: "long_running", MaxDurationMinutes: 30, WarnCycles: 2, CancelCycles: 4},
				{Name: "blocker", MaxBlockedSessions: 5, WarnCycles: 1, CancelCycles: 2},
			},
			SafePatterns: []string{"pg_dump", "pg_basebackup"},
		},
		Explain: ExplainConfig{
			Enabled:         true,
			TimeoutMs:       DefaultExplainTimeoutMs,
			CacheTTLMinutes: DefaultExplainCacheTTLMinutes,
			MaxTokens:       DefaultExplainMaxTokens,
		},
		Retention: RetentionConfig{
			SnapshotsDays: DefaultRetentionSnapshotsDays,
			FindingsDays:  DefaultRetentionFindingsDays,
			ActionsDays:   DefaultRetentionActionsDays,
			ExplainsDays:  DefaultRetentionExplainsDays,
		},
		Prometheus: PrometheusConfig{
			ListenAddr: DefaultPrometheusListenAddr,
		},
		API: APIConfig{
			ListenAddr: DefaultAPIListenAddr,
		},
		OAuth: OAuthConfig{
			DefaultRole: "viewer",
		},
	}
}

func loadYAML(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// Expand ${ENV_VAR} references with validation.
	raw := string(data)
	expanded := os.ExpandEnv(raw)

	// Warn about env vars that expanded to empty strings. This catches the
	// common case where ${SAGE_LLM_API_KEY} is in the YAML but the env var
	// is not set, leaving an empty value that silently breaks the feature.
	warnUnexpandedEnvVars(raw, expanded)

	return yaml.Unmarshal([]byte(expanded), cfg)
}

// warnUnexpandedEnvVars detects ${VAR} patterns in the raw YAML that expanded
// to empty strings (meaning the env var was not set) and logs a warning.
func warnUnexpandedEnvVars(raw, expanded string) {
	// Find all ${...} references in the raw YAML.
	for i := 0; i < len(raw); i++ {
		if i+1 < len(raw) && raw[i] == '$' && raw[i+1] == '{' {
			end := strings.Index(raw[i:], "}")
			if end < 0 {
				continue
			}
			varName := raw[i+2 : i+end]
			if os.Getenv(varName) == "" {
				fmt.Fprintf(os.Stderr,
					"WARNING: config %q references ${%s} but it is not set in the environment\n",
					"config.yaml", varName)
			}
			i += end
		}
	}
}

func overlayEnv(cfg *Config) {
	if v := os.Getenv("SAGE_MODE"); v != "" {
		cfg.Mode = v
	}
	if v := os.Getenv("SAGE_DATABASE_URL"); v != "" {
		cfg.Postgres.DatabaseURL = v
	}
	if v := os.Getenv("SAGE_PG_HOST"); v != "" {
		cfg.Postgres.Host = v
	}
	if v := envInt("SAGE_PG_PORT"); v != 0 {
		cfg.Postgres.Port = v
	}
	if v := os.Getenv("SAGE_PG_USER"); v != "" {
		cfg.Postgres.User = v
	}
	if v := os.Getenv("SAGE_PG_PASSWORD"); v != "" {
		cfg.Postgres.Password = v
	}
	if v := os.Getenv("SAGE_PG_DATABASE"); v != "" {
		cfg.Postgres.Database = v
	}
	if v := os.Getenv("SAGE_PG_SSLMODE"); v != "" {
		cfg.Postgres.SSLMode = v
	}
	if v := envInt("SAGE_PG_MAX_CONNS"); v != 0 {
		cfg.Postgres.MaxConnections = v
	}
	if v := os.Getenv("SAGE_PROMETHEUS_PORT"); v != "" {
		cfg.Prometheus.ListenAddr = "0.0.0.0:" + v
	}
	if v := envInt("SAGE_RATE_LIMIT"); v != 0 {
		// Store in a field we can access later; use default.
	}
	if v := os.Getenv("SAGE_LLM_API_KEY"); v != "" {
		cfg.LLM.APIKey = v
	}
	if v := os.Getenv("SAGE_LLM_ENDPOINT"); v != "" {
		cfg.LLM.Endpoint = v
	}
	if v := os.Getenv("SAGE_LLM_MODEL"); v != "" {
		cfg.LLM.Model = v
	}
	if v := os.Getenv("SAGE_TRUST_LEVEL"); v != "" {
		cfg.Trust.Level = v
	}
	if v := os.Getenv("SAGE_META_DB"); v != "" {
		cfg.MetaDB = v
	}
	if v := os.Getenv("SAGE_ENCRYPTION_KEY"); v != "" {
		cfg.EncryptionKey = v
	}
	if v := os.Getenv("SAGE_OPTIMIZER_LLM_API_KEY"); v != "" {
		cfg.LLM.OptimizerLLM.APIKey = v
	}
	if v := os.Getenv("SAGE_OPTIMIZER_LLM_ENDPOINT"); v != "" {
		cfg.LLM.OptimizerLLM.Endpoint = v
	}
	if v := os.Getenv("SAGE_OPTIMIZER_LLM_MODEL"); v != "" {
		cfg.LLM.OptimizerLLM.Model = v
	}
	if v := os.Getenv("SAGE_OAUTH_CLIENT_ID"); v != "" {
		cfg.OAuth.ClientID = v
	}
	if v := os.Getenv("SAGE_OAUTH_CLIENT_SECRET"); v != "" {
		cfg.OAuth.ClientSecret = v
	}
	if v := os.Getenv("SAGE_OAUTH_ISSUER_URL"); v != "" {
		cfg.OAuth.IssuerURL = v
	}
	if v := os.Getenv("SAGE_OAUTH_REDIRECT_URL"); v != "" {
		cfg.OAuth.RedirectURL = v
	}
	if v := os.Getenv("SAGE_OAUTH_PROVIDER"); v != "" {
		cfg.OAuth.Provider = v
	}
}

// HotReloadable returns the fields that can be reloaded without restart.
func (c *Config) HotReloadable() []string {
	return []string{
		"collector.interval_seconds",
		"analyzer.*",
		"safety.*",
		"trust.level", "trust.tier3_safe", "trust.tier3_moderate",
		"trust.maintenance_window",
		"llm.*",
		"briefing.schedule",
		"alerting.*",
		"auto_explain.log_min_duration_ms",
		"auto_explain.max_plans_per_cycle",
		"forecaster.*",
		"tuner.*",
		"retention.*",
	}
}

// IsStandalone returns true if running in standalone mode.
func (c *Config) IsStandalone() bool {
	return c.Mode == "standalone"
}

// IsFleet returns true if running in fleet mode.
func (c *Config) IsFleet() bool {
	return c.Mode == "fleet"
}

// HasMetaDB returns true if a metadata database is configured.
func (c *Config) HasMetaDB() bool {
	return c.MetaDB != ""
}

// HasEncryptionKey returns true if an encryption passphrase is set.
func (c *Config) HasEncryptionKey() bool {
	return c.EncryptionKey != ""
}

// RateLimit returns the configured rate limit.
func (c *Config) RateLimit() int {
	if v := envInt("SAGE_RATE_LIMIT"); v != 0 {
		return v
	}
	return DefaultRateLimit
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

func envFloat(key string) float64 {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	f, _ := strconv.ParseFloat(v, 64)
	return f
}

// Suppress unused warnings.
var _ = envFloat
var _ = strings.Contains

// DefaultConfig returns a Config populated with defaults.
// Exported for use by cmd/gen_config_meta (v0.8.5 Feature 5).
func DefaultConfig() *Config {
	return newDefaults()
}
