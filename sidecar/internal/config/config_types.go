package config

import "time"

// Config is the top-level configuration for pg_sage standalone sidecar.
//
// Hot-reload mutations (from the /api/v1/config API) must be serialized
// against other mutations and against background-loop reads. Mutations
// call cfg.Lock()/cfg.Unlock(); readers that require a self-consistent
// view of multi-field state should use cfg.RLock()/cfg.RUnlock().
// Single-field reads of naturally-aligned scalars may skip locking on
// most architectures but will still trip the race detector — new code
// should prefer the explicit lock.
type Config struct {
	Mode string `yaml:"mode" doc:"Operating mode: standalone, fleet, or meta. Standalone connects to one PostgreSQL target; fleet manages many databases from one sidecar."`

	Postgres    PostgresConfig      `yaml:"postgres"`
	Collector   CollectorConfig     `yaml:"collector"`
	Analyzer    AnalyzerConfig      `yaml:"analyzer"`
	Safety      SafetyConfig        `yaml:"safety"`
	Trust       TrustConfig         `yaml:"trust"`
	LLM         LLMConfig           `yaml:"llm"`
	Advisor     AdvisorConfig       `yaml:"advisor"`
	Briefing    BriefingConfig      `yaml:"briefing"`
	Alerting    AlertingConfig      `yaml:"alerting"`
	AutoExplain AutoExplainConfig   `yaml:"auto_explain"`
	Forecaster  ForecasterConfig    `yaml:"forecaster"`
	Tuner       TunerConfig         `yaml:"tuner"`
	Tuning      TuningConfig        `yaml:"tuning"`
	RCA         RCAConfig           `yaml:"rca"`
	SRE         SREConfig           `yaml:"sre"`
	Runaway     RunawayConfig       `yaml:"runaway"`
	Explain     ExplainConfig       `yaml:"explain"`
	LogWatch    LogWatchConfig      `yaml:"logwatch"`
	SchemaLint  SchemaLintConfig    `yaml:"schema_lint"`
	Migration   MigrationConfig     `yaml:"migration"`
	Rehearsal   RehearsalConfig     `yaml:"rehearsal"`
	Retention   RetentionConfig     `yaml:"retention"`
	Prometheus  PrometheusConfig    `yaml:"prometheus"`
	Azure       AzureConfig         `yaml:"azure"`
	OAuth       OAuthConfig         `yaml:"oauth"`
	Policy      PolicyConfig        `yaml:"policy"`
	Value       ValueConfig         `yaml:"value"`
	Verify      VerifyConfig        `yaml:"verify"`
	Clone       CloneProviderConfig `yaml:"clone"`
	Custodian   CustodianConfig     `yaml:"custodian"`
	MCP         MCPConfig           `yaml:"mcp"`
	Ask         AskConfig           `yaml:"ask"`
	Facts       FactsConfig         `yaml:"facts"`
	SelfBudget  SelfBudgetConfig    `yaml:"self_budget"`
	Baseline    BaselineConfig      `yaml:"baseline"`
	History     HistoryConfig       `yaml:"history"`
	Debug       DebugConfig         `yaml:"debug"`
	SelfConfig  SelfConfigConfig    `yaml:"self_config"`
	// Specialist is the Postgres-specialist contract other agents call.
	Specialist SpecialistConfig `yaml:"specialist"`
	// Budgets are the workload budgets (workload_budgets.go, market-08).
	Budgets []BudgetConfig `yaml:"budgets" doc:"Workload budgets per role, application_name or sqlcommenter tag: warn mode (default) counts would-be violations; enforce (role budgets) proposes ALTER ROLE settings. Default: none."`
	// FleetLearning is fleet learning and leader election (fleet_learning.go).
	FleetLearning FleetLearningConfig `yaml:"fleet_learning"`
	// Fleet holds fleet auto-discovery (fleet_discovery.go, market-06).
	Fleet FleetConfig `yaml:"fleet"`
	// HA is each database's HA monitor cadence (ha.go).
	HA HAConfig `yaml:"ha"`
	// Backup is the backup freshness policy (backup.go, market-07).
	Backup BackupConfig `yaml:"backup"`
	// Drill is the fleet-wide restore drill cap (drill.go, drill-01a).
	Drill DrillConfig `yaml:"drill"`
	// CloudTelemetry is managed-cloud host telemetry (cloud_telemetry.go).
	CloudTelemetry        CloudTelemetryConfig        `yaml:"cloud_telemetry"`
	LakebaseObservability LakebaseObservabilityConfig `yaml:"lakebase_observability"`
	// ProviderAdvice is provider advisor recommendations (provider_advice.go).
	ProviderAdvice ProviderAdviceConfig `yaml:"provider_advice"`
	// Cost is operator-supplied pricing (cost.go, market-04).
	Cost CostConfig `yaml:"cost"`
	// Agents is agent posture (agents.go).
	Agents AgentsConfig `yaml:"agents"`
	// Audit is the audit export and evidence of E2 (audit.go).
	Audit AuditConfig `yaml:"audit"`
	// Replication is CDC consumer health (replication.go, market-13).
	Replication ReplicationConfig `yaml:"replication"`

	// NotificationPolicy governs notification channel targets (G7-B21). The
	// top-level "notifications" key is retired (see rejectRetiredTopLevelConfig).
	NotificationPolicy NotificationPolicyConfig `yaml:"notification_policy"`

	// Checks are owner-defined read-only checks (checks.go, market-11).
	Checks []CustomCheck `yaml:"checks" doc:"Owner-defined read-only checks: one SELECT each, run read-only; each row is a custom:<id> finding (docs/custom-checks.md). Default: empty."`

	// Fleet mode fields.
	Databases []DatabaseConfig `yaml:"databases"`
	Defaults  DefaultsConfig   `yaml:"defaults"`
	API       APIConfig        `yaml:"api"`

	// Meta database and encryption (--meta-db, --encryption-key).
	MetaDB        string `yaml:"meta_db" doc:"DSN of the metadata database used in fleet mode to persist cross-target state. Blank in standalone mode."`
	EncryptionKey string `yaml:"encryption_key" doc:"Passphrase that encrypts stored secrets: per-database passwords, channel secrets and API-set config secrets. Also SAGE_ENCRYPTION_KEY(_FILE)." secret:"true"`
	// EncryptionKeyPrevious opens config secrets sealed under the key
	// before a rotation; startup re-seals them under encryption_key.
	EncryptionKeyPrevious string `yaml:"encryption_key_previous" doc:"Previous encryption_key during a rotation: config secrets sealed under it are re-encrypted at startup. Remove once rotated." secret:"true"`
	// DataDir is a persistent directory pg_sage owns. Without an
	// encryption_key, startup generates <data_dir>/encryption.key there.
	DataDir string `yaml:"data_dir" doc:"Persistent directory owned by pg_sage. Without encryption_key, pg_sage generates data_dir/encryption.key (mode 600) and seals stored secrets with it; back the file up. Also SAGE_DATA_DIR."`

	// Legacy env-var fields
	APIKey  string `yaml:"-"`
	TLSCert string `yaml:"-"`
	TLSKey  string `yaml:"-"`

	// Runtime (not from config file)
	ConfigPath         string `yaml:"-"`
	PGVersionNum       int    `yaml:"-"` // e.g. 160000 for PG16
	HasWALColumns      bool   `yaml:"-"`
	HasPlanTimeColumns bool   `yaml:"-"`
	StatementsSchema   string `yaml:"-"` // pg_stat_statements' schema; empty: not found
	CloudEnvironment   string `yaml:"-"` // e.g. "rds", "cloud-sql", "self-managed"

	// The file layer's presence set and the overrides masking it (config-03,
	// explicit_paths.go). Both are immutable once set, so Clone shares them.
	explicitPaths map[string]struct{}
	overrideMask  *overrideMask
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
	ActionDSN      string `yaml:"action_dsn" doc:"Optional DSN of a second role on the same database. Only executor actions and backend cancel/terminate use it (a lazy pool of at most 7); the main role only observes. Empty: one role." secret:"true"`
	Provider       string `yaml:"provider" doc:"Optional provider override, e.g. rds, cloud-sql, neon or self-managed. Used only while detection cannot tell; a disagreement with detection keeps the detected provider and warns."`
	// CloudResource names the provider resource when the host does not.
	CloudResource CloudResourceConfig `yaml:"cloud_resource"`
	// CNPG names the CloudNativePG Cluster behind the database (market-12).
	CNPG CNPGConfig `yaml:"cnpg"`
}

type CollectorConfig struct {
	IntervalSeconds int `yaml:"interval_seconds" api:"int_min5"`
	BatchSize       int `yaml:"batch_size" api:"int_pos"`
	MaxQueries      int `yaml:"max_queries" api:"int_pos"`
}

type AnalyzerConfig struct {
	IntervalSeconds              int     `yaml:"interval_seconds" doc:"How often the analyzer runs its rule checks, in seconds. Lower values detect issues faster but add load." api:"int_min5"`
	SlowQueryThresholdMs         int     `yaml:"slow_query_threshold_ms" doc:"Queries whose mean execution time over the last collector interval is above this are considered slow and become candidates for optimization findings." api:"int_nonneg"`
	SlowQueryMinCalls            int     `yaml:"slow_query_min_calls" doc:"Fewest calls a query must make in one collector interval before its interval mean is judged by slow_query and query_regression. 0 uses the default. Default: 5."`
	SeqScanMinRows               int     `yaml:"seq_scan_min_rows" doc:"Minimum table row count before a sequential scan is reported as a finding. Below this, a seq scan is usually cheaper than an index lookup." api:"int_pos"`
	UnusedIndexWindowDays        int     `yaml:"unused_index_window_days" doc:"An index must be unused (idx_scan=0) for at least this many days before the analyzer suggests dropping it. Default 7 days." warning:"Setting this too low can recommend dropping indexes that support infrequent but critical queries (e.g. monthly reports)." api:"int_pos"`
	IndexBloatThresholdPct       int     `yaml:"index_bloat_threshold_pct" doc:"Estimated btree bloat percentage (0-100) for an index_bloat finding recommending REINDEX INDEX CONCURRENTLY: info from it, warning from twice it. 0 uses the default. Default: 30." api:"pct"`
	IndexBloatMinBytes           int64   `yaml:"index_bloat_min_bytes" doc:"Smallest btree index (bytes, from pg_class.relpages) whose bloat the collector estimates from planner statistics each cycle, at most the 200 largest. 0 uses the default. Default: 104857600 (100 MiB)."`
	TableBloatDeadTuplePct       int     `yaml:"table_bloat_dead_tuple_pct" doc:"Dead-tuple percentage threshold for reporting table bloat. Above this the analyzer emits a vacuum or repack finding." api:"pct"`
	TableBloatMinRows            int     `yaml:"table_bloat_min_rows" doc:"Minimum row count before a table is considered for bloat analysis. Tiny tables are noisy and not worth reporting."`
	TableBloatMinBytes           int64   `yaml:"table_bloat_min_bytes" doc:"Minimum heap size in bytes before the freeze custodian plans an online bloat rebuild from the catalog bloat estimate. 0 uses the default. Default: 1073741824 (1 GiB)."`
	AutovacuumTuneMinRows        int     `yaml:"autovacuum_tune_min_rows" doc:"Minimum live-row count before the per-table autovacuum tuner recommends a lower scale factor. Tuning small tables is pointless. Default: 1000000." api:"int_pos"`
	AnalyzeStaleMinRows          int     `yaml:"analyze_stale_min_rows" doc:"Minimum live rows before recommending ANALYZE on a stale/never-analyzed table. Default: 10000." api:"int_pos"`
	AnalyzeStaleDays             int     `yaml:"analyze_stale_days" doc:"A table whose last (auto)analyze is older than this many days, with write activity, is flagged for ANALYZE. Default: 7." api:"int_pos"`
	WraparoundFreezeShare        float64 `yaml:"wraparound_freeze_share" doc:"Share (0-1] of a table's effective freeze maximum (setting or lower reloption) at which the analyzer reports wraparound risk; the freeze custodian decides the VACUUM. Default: 0.75." api:"float_share"`
	IdleInTxTimeoutMinutes       int     `yaml:"idle_in_transaction_timeout_minutes" doc:"Report sessions sitting in 'idle in transaction' longer than this. Long idle-in-tx sessions block vacuum and hold locks."`
	CacheHitRatioWarning         float64 `yaml:"cache_hit_ratio_warning" doc:"Warn when the shared buffers cache hit ratio falls below this fraction (0.0-1.0). Low ratios suggest shared_buffers is under-sized or working set is too large." api:"float01"`
	XIDWraparoundWarning         int64   `yaml:"xid_wraparound_warning" doc:"Transaction age at which the analyzer emits a wraparound warning. Normally ~200 million."`
	XIDWraparoundCritical        int64   `yaml:"xid_wraparound_critical" doc:"Transaction age at which the analyzer escalates to critical severity. Normally ~1.5 billion — past this autovacuum will force emergency wraparound runs." warning:"If this is hit, autovacuum enters aggressive mode and the database may eventually refuse writes."`
	RegressionThresholdPct       int     `yaml:"regression_threshold_pct" doc:"Percent latency regression required before a query is flagged as a regressor. Set low for sensitive detection, higher to reduce noise." api:"pct"`
	RegressionLookbackDays       int     `yaml:"regression_lookback_days" doc:"How many days of pg_stat_statements history the regression detector compares against."`
	CheckpointFreqWarningPerHour int     `yaml:"checkpoint_frequency_warning_per_hour" doc:"Warn when checkpoints occur more than this many times per hour. High frequency indicates max_wal_size is too small."`

	// v0.8.5 Feature 3 — work_mem role-promotion advisor.
	// Threshold: how many hint applications in a 24h window on a single
	// role/query_hash before promoting to a role-level work_mem finding.
	WorkMemPromotionThreshold int `yaml:"work_mem_promotion_threshold" doc:"How many active work_mem hints a single role must accumulate before the analyzer recommends promoting work_mem at the role level via ALTER ROLE. Set 0 to disable the advisor."`

	// v0.9.2 — Slow active replication slot threshold (bytes).
	SlowSlotRetainedBytes int64 `yaml:"slow_slot_retained_bytes" doc:"Active slots retaining more WAL than this (bytes) are flagged as slow consumers. Default 1 GB. Slow consumers hold xmin, blocking vacuum." api:"int_pos" range:"1,1099511627776" zero:"default"`

	// InactiveSlotMinMinutes: see inactive_slot.go.
	InactiveSlotMinMinutes int `yaml:"inactive_slot_min_minutes" doc:"Report a replication slot once it has been inactive this many minutes (wal_status extended, unreserved or lost reports it at once). 0 uses the default. Range 0-10080. Default: 5." api:"int_week_minutes" zero:"default"`

	// Subscriber and CDC health thresholds: see replication.go (market-13).
	SubscriptionLagMinutes int `yaml:"subscription_lag_minutes" doc:"Raise subscription_lag when a subscription's last report to its publisher (pg_stat_subscription.latest_end_time) is this many minutes old. 0 uses the default. Range 0-10080. Default: 5."`
	CDCStallMinutes        int `yaml:"cdc_stall_minutes" doc:"Raise cdc_consumer_stalled when WAL advanced but an active logical slot's confirmed_flush_lsn did not move for this many minutes. 0 uses the default. Range 0-10080. Default: 10."`

	// v0.9 — Lock chain detection.
	LockChain LockChainConfig `yaml:"lock_chain"`

	// Schema guard post-DDL debounce: see schema_guard.go.
	SchemaGuardDDLDebounceSeconds int `yaml:"schema_guard_ddl_debounce_seconds" doc:"After pg_sage's own DDL, the schema guard re-scans the schema at most once per this many seconds (DDL bursts are coalesced into one extra scan). 0 uses the default. Range 0-3600. Default: 60."`

	// SelfCostBudgetMs: see self_cost.go.
	SelfCostBudgetMs int `yaml:"self_cost_budget_ms" doc:"Raise a sage_self_cost finding when pg_sage's own statements use more than this many ms of database time per collector cycle. 0 disables the finding. Range 0-3600000. Default: 3000." api:"int_nonneg" range:"0,3600000"`

	// DisabledRules: see disabled_rules.go (market-11).
	DisabledRules []string `yaml:"disabled_rules" doc:"Analyzer rules or categories to skip; their open findings resolve as rule_disabled. The wraparound, sequence and sage_* rules cannot be disabled. Default: empty." api:"rule_list"`
}

type SafetyConfig struct {
	CPUCeilingPct            int `yaml:"cpu_ceiling_pct" doc:"Skip collector cycles when active client sessions / max_connections exceeds this percent (1-100), so pg_sage adds no load during incidents. Verify inherits it while host_cpu_ceiling_pct is unset." api:"pct1_100"`
	HostCPUCeilingPct        int `yaml:"host_cpu_ceiling_pct" doc:"Index verification applies a change only while database-host CPU is at or below this percent (1-100). Unset (0) inherits cpu_ceiling_pct, default 90." api:"pct1_100" zero:"default"`
	QueryTimeoutMs           int `yaml:"query_timeout_ms" doc:"statement_timeout applied to every collector and analyzer query issued by the sidecar. Protects the target from runaway introspection." api:"int_pos"`
	DDLTimeoutSeconds        int `yaml:"ddl_timeout_seconds" doc:"Timeout applied to DDL actions (CREATE INDEX, ALTER TABLE) executed by the executor. Per-statement ceiling, not per-cycle." api:"int_pos"`
	DiskPressureThresholdPct int `yaml:"disk_pressure_threshold_pct" doc:"When database data volume usage exceeds this percent, skip write-producing actions. Prevents adding indexes to a near-full volume."`
	BackoffConsecutiveSkips  int `yaml:"backoff_consecutive_skips" doc:"After this many consecutive skipped cycles (e.g. CPU ceiling hit repeatedly), enter dormant mode and slow the cadence until load subsides."`
	DormantIntervalSeconds   int `yaml:"dormant_interval_seconds" doc:"Cycle interval (seconds) used while in dormant mode — typically much larger than the normal interval so the sidecar wakes rarely while the target is stressed."`
	LockTimeoutMs            int `yaml:"lock_timeout_ms" doc:"lock_timeout applied to connections running DDL or ANALYZE. Must be > 0 for ANALYZE actions in v0.8.5 — autovacuum can hold a ShareUpdateExclusiveLock indefinitely." warning:"Zero disables the timeout entirely and is refused by the executor for ANALYZE actions." api:"int_pos"`

	// IO ceilings for declared-capacity load admission (D6), independent
	// of cpu_ceiling_pct.
	DataIOCeilingPct int `yaml:"data_io_ceiling_pct" doc:"Data IO ceiling, % of capacity."`
	WALIOCeilingPct  int `yaml:"wal_io_ceiling_pct" doc:"WAL IO ceiling, % of capacity."`

	// Per-class statement timeouts (executor-05): see ddl_class_timeouts.go.
	IndexBuildTimeoutSeconds int `yaml:"index_build_timeout_seconds" doc:"statement_timeout of a CREATE INDEX CONCURRENTLY run by the executor (seconds). Other DDL keeps ddl_timeout_seconds. 0 uses the default. Range 60-86400. Default: 14400." api:"int_class_timeout" zero:"default"`
	ReindexTimeoutSeconds    int `yaml:"reindex_timeout_seconds" doc:"statement_timeout of a REINDEX CONCURRENTLY run by the executor (seconds). 0 uses the default. Range 60-86400. Default: 14400." api:"int_class_timeout" zero:"default"`
	VacuumTimeoutSeconds     int `yaml:"vacuum_timeout_seconds" doc:"Cap on a VACUUM (not VACUUM FULL) run by the executor (seconds), enforced by its watchdog; the VACUUM runs with statement_timeout 0. 0 uses the default. Range 60-86400. Default: 14400." api:"int_class_timeout" zero:"default"`
	// VACUUM watchdog (vacuum-05): see vacuum_stall.go.
	VacuumStallMinutes int `yaml:"vacuum_stall_minutes" doc:"Minutes an executor VACUUM may show no heap-phase progress before its watchdog cancels it (vacuum_stalled). 0 uses the default. Range 5-240. Default: 15." api:"int_vacuum_stall" zero:"default"`
}

type TrustConfig struct {
	Level                 string `yaml:"level" doc:"Operator ceiling and kill switch, never a grant: the ledger grants, the operator caps. observation: cases only; advisory: earned safe actions, moderate to approval; autonomous: both." warning:"Changing trust can expand what execution_mode=auto may execute without human review." api:"trust_level" enum:"observation|advisory|autonomous"`
	RampStart             string `yaml:"ramp_start" doc:"RFC3339 time pg_sage began observing the database; auto-persisted on first startup if empty. Observation time is the floor before a trust promotion can be proposed."`
	RampSafeHours         int    `yaml:"ramp_safe_hours" doc:"Minimum hours observed before pg_sage may propose a promotion to L2 (or L3 for SAFE classes): a floor, never a grant. 1-8760. Default: 192. Irreversible actions: at least 192." warning:"Lower values let pg_sage propose promotions sooner after first start; the sidecar warns at startup."`
	RampModerateHours     int    `yaml:"ramp_moderate_hours" doc:"Minimum hours observed before pg_sage may propose a MODERATE class's promotion to L3: a floor, never a grant. 1-8760, >= ramp_safe_hours. Default: 744. Irreversible: at least 744." warning:"Lower values let pg_sage propose promotions sooner after first start; the sidecar warns at startup."`
	MaintenanceWindow     string `yaml:"maintenance_window" doc:"Window for MODERATE auto-actions: always, never, presets (nights, weeknights, weekends), Mon-Fri 01:00-05:00, or cron (1h wide, or @30m). Optional IANA zone: weeknights America/Chicago." example:"weeknights" api:"maintenance_window"`
	Tier3Safe             bool   `yaml:"tier3_safe" doc:"Operator switch for unattended SAFE actions (ANALYZE, non-FULL VACUUM) the trust ledger has earned; off caps them at approval. Needs execution_mode=auto and trust.level advisory+." api:"bool"`
	Tier3Moderate         bool   `yaml:"tier3_moderate" doc:"Operator switch for unattended MODERATE actions (CREATE INDEX CONCURRENTLY) the trust ledger has earned; off caps them at approval. Needs execution_mode=auto, trust.level=autonomous." warning:"Moderate actions can consume IO or briefly contend for locks." api:"bool"`
	Tier3HighRisk         bool   `yaml:"tier3_high_risk" doc:"Enable tier-3 actions classified as high risk. Ignored outside fleet mode — standalone always forces this to false." mode:"fleet-only" warning:"High-risk actions can destabilize production — enable only with a reviewed rollback plan." api:"bool"`
	RollbackThresholdPct  int    `yaml:"rollback_threshold_pct" doc:"Latency regression percentage (0-100) that triggers automatic rollback of a recently applied action. Lower = more sensitive." api:"pct"`
	RollbackWindowMinutes int    `yaml:"rollback_window_minutes" doc:"How many minutes after applying an action pg_sage watches for regressions before considering it stable." api:"int_pos"`
	RollbackCooldownDays  int    `yaml:"rollback_cooldown_days" doc:"After a rollback, the same action family is blocked for this many days to prevent flap. Default 3 days." api:"int_pos"`
	CascadeCooldownCycles int    `yaml:"cascade_cooldown_cycles" doc:"If N consecutive cycles each propose new actions, throttle to observe impact. Prevents storm-like cascades of small changes." api:"int_pos" zero:"default"`
}

type LLMConfig struct {
	Auth                   string               `yaml:"auth" doc:"LLM authentication: api_key (default) or databricks (SDK unified authentication with automatic token refresh). Databricks auth requires the configured workspace HTTPS origin." api:"llm_auth" enum:"|api_key|databricks"`
	Enabled                bool                 `yaml:"enabled" doc:"Global enable for LLM-assisted analysis. On by default; calls a provider only once llm.endpoint and llm.api_key are set. When off, pg_sage uses deterministic heuristics only." api:"bool"`
	Endpoint               string               `yaml:"endpoint" doc:"LLM provider base URL. Supports OpenAI-compatible chat-completions endpoints. Can be left blank to use the vendor default." api:"string"`
	APIKey                 string               `yaml:"api_key" doc:"Authentication token for the LLM provider. Prefer sourcing from environment via ${LLM_API_KEY} rather than committing literal values." secret:"true" api:"string"`
	Model                  string               `yaml:"model" doc:"Model identifier sent to the provider (e.g. gpt-4o-mini). Must support JSON-mode responses for structured outputs." api:"string"`
	TimeoutSeconds         int                  `yaml:"timeout_seconds" doc:"Timeout of each LLM request attempt; 429 and 5xx replies are retried, up to 4 attempts. Reasoning models get at least 120 s per attempt. Low values protect the analyzer cycle." api:"int_pos"`
	TokenBudgetDaily       int                  `yaml:"token_budget_daily" doc:"Daily token cap (input + output) per LLM client: the shared client and each database's general and optimizer client get their own copy. llm.global_token_budget_daily caps the sum." api:"int_nonneg"`
	FleetTokenBudgetDaily  int                  `yaml:"fleet_token_budget_daily" doc:"Fleet-wide daily token cap split per database so one noisy database can't drain the whole budget. 0 disables per-database budgeting (fleet mode only)." api:"int_nonneg"`
	GlobalTokenBudgetDaily int                  `yaml:"global_token_budget_daily" doc:"Process-wide daily token cap across all LLM clients. 0 derives today's total: the clients' token_budget_daily summed, an active fleet budget standing in for the per-database ones." api:"int_nonneg"`
	ContextBudgetTokens    int                  `yaml:"context_budget_tokens" doc:"Trim target in tokens (~4 bytes each) for prompt context: lowering it shrinks advisor, tuning and explain prompts, each keeping its own cap. 0 turns off this extra trim. Never refuses or caps answers." api:"int_nonneg"`
	CooldownSeconds        int                  `yaml:"cooldown_seconds" doc:"Dedup window and breaker cooldown: an identical request is not re-sent within this many seconds; after 3 failures in a row (or one 401/403) calls pause this long, then one probe decides."`
	JSONMode               bool                 `yaml:"json_mode" doc:"When true, requests structured JSON via response_format: json_object. Supported by OpenAI, Gemini (OpenAI-compat), Groq, Ollama. Off for providers that reject unknown fields." api:"bool"`
	TokenParameter         string               `yaml:"token_parameter" doc:"Completion cap parameter: auto (max_tokens, switching per model to max_completion_tokens when the provider asks), max_tokens or max_completion_tokens. Default: auto."`
	ToolReasoningEffort    string               `yaml:"tool_reasoning_effort" doc:"reasoning_effort sent with tool calls: auto (unset; none once a model rejects tools without it), none, low, medium, high, or omit (never sent). Default: auto."`
	IndexOptimizer         IndexOptimizerConfig `yaml:"index_optimizer"` // Deprecated: use Optimizer.
	Optimizer              OptimizerConfig      `yaml:"optimizer"`
	OptimizerLLM           OptimizerLLMConfig   `yaml:"optimizer_llm"`
	RedactNumericLiterals  bool                 `yaml:"redact_numeric_literals" doc:"Replace compared numbers of 6+ digits (=, <>, <, >, IN, ANY, BETWEEN, Key (c)=(...)) with ? before SQL, plan or log text reaches a model. Short numbers are kept. Default: true."`
	Pricing                map[string]LLMPrice  `yaml:"pricing" doc:"Optional USD prices per million tokens by model id: {model: {input_per_m, output_per_m, reasoning_per_m}}. Empty by default; an unpriced model gets a NULL cost, never a guess."`
}

type IndexOptimizerConfig struct {
	Enabled            bool `yaml:"enabled"`
	MinQueryCalls      int  `yaml:"min_query_calls"`
	MaxIndexesPerTable int  `yaml:"max_indexes_per_table"`
	MaxIncludeColumns  int  `yaml:"max_include_columns"`
	WriteHeavyRatio    int  `yaml:"write_heavy_ratio_pct"`
}

// OptimizerConfig controls the index optimizer v2 behavior.
type OptimizerConfig struct {
	Enabled              bool    `yaml:"enabled" doc:"Allow index creates and drops: the tuning agent (needs an LLM) and, without one, the deterministic index floor. Changes execute only via the policy gate and trust level. Default: true." api:"bool"`
	MinQueryCalls        int     `yaml:"min_query_calls" api:"int_pos"`
	MaxIndexesPerTable   int     `yaml:"max_indexes_per_table"`
	MaxNewPerTable       int     `yaml:"max_new_per_table" api:"int_pos"`
	MaxIncludeColumns    int     `yaml:"max_include_columns" doc:"Most INCLUDE columns a proposed index may carry; a wider proposal is refused with a reason naming the cap. 0 or less means 3. Default: 3."`
	WriteHeavyRatioPct   int     `yaml:"write_heavy_ratio_pct"`
	MinSnapshots         int     `yaml:"min_snapshots"`
	HypoPGMinImprovePct  float64 `yaml:"hypopg_min_improvement_pct"`
	ConfidenceThreshold  float64 `yaml:"confidence_threshold"`
	WriteImpactThreshPct float64 `yaml:"write_impact_threshold_pct"`
	// RejectionMemory: optimizer_rejection_memory.go.
	RejectionMemory OptimizerRejectionMemoryConfig `yaml:"rejection_memory"`
}

// OptimizerLLMConfig configures the dedicated optimizer LLM (reasoning-tier).
type OptimizerLLMConfig struct {
	Enabled           bool   `yaml:"enabled" doc:"Enable the dedicated reasoning-tier LLM used by the index optimizer. When false the optimizer falls back to the general llm.* client. Default: false."`
	Endpoint          string `yaml:"endpoint" doc:"Base URL for the optimizer LLM provider. Can differ from the general llm.endpoint when the reasoning tier lives on another provider."`
	APIKey            string `yaml:"api_key" doc:"API key for the optimizer LLM. Prefer env-var substitution over literal values." secret:"true"`
	Model             string `yaml:"model" doc:"Reasoning-tier model identifier (e.g. o1-mini). Must return JSON when requested."`
	TimeoutSeconds    int    `yaml:"timeout_seconds" doc:"Per-request timeout for optimizer LLM calls. Reasoning models typically need longer timeouts than chat models."`
	TokenBudgetDaily  int    `yaml:"token_budget_daily" doc:"Daily token cap for the optimizer LLM — independent of the general llm.token_budget_daily so reasoning spend can be tracked separately."`
	CooldownSeconds   int    `yaml:"cooldown_seconds" doc:"As llm.cooldown_seconds, for the optimizer client: an identical request is not re-sent within this many seconds, and the breaker pauses calls this long after 3 failures. 0 uses llm.cooldown_seconds."`
	MaxOutputTokens   int    `yaml:"max_output_tokens" doc:"Upper bound on output tokens accepted from the optimizer LLM. Truncates runaway responses."`
	FallbackToGeneral bool   `yaml:"fallback_to_general" doc:"If true, fall back to the general llm.* client when the optimizer LLM is unavailable or over budget."`
}

type AdvisorConfig struct {
	Enabled           bool `yaml:"enabled" doc:"Run the LLM configuration advisor. On by default; needs a usable LLM at startup. Its changes execute only through the policy gate and trust level. Default: true." api:"bool"`
	IntervalSeconds   int  `yaml:"interval_seconds" api:"int_min5"`
	VacuumEnabled     bool `yaml:"vacuum_enabled" doc:"Let the tuning agent propose table storage parameters (autovacuum, fillfactor) for its write-amplification cases. Needs advisor.enabled. Default: true."`
	WALEnabled        bool `yaml:"wal_enabled"`
	ConnectionEnabled bool `yaml:"connection_enabled"`
	MemoryEnabled     bool `yaml:"memory_enabled" doc:"Let the tuning agent propose server settings (work_mem and other memory, planner and autovacuum settings) for its cases. Needs advisor.enabled. Default: true."`
	RewriteEnabled    bool `yaml:"rewrite_enabled"`
	BloatEnabled      bool `yaml:"bloat_enabled"`
	BudgetProposals   bool `yaml:"budget_proposals" doc:"Propose a workload budget statement_timeout per role from its statement time (3x the 14-day p99, rounded up, never below the observed maximum) as info findings. Default: true."`
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
