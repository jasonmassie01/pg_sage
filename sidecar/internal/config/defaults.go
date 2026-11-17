package config

import "time"

// Default values matching the spec.
const (
	DefaultMode = "standalone"
	// ModeMeta labels a meta-db fleet (--meta-db with no explicit mode).
	ModeMeta = "meta"

	DefaultPGPort           = 5432
	DefaultPGUser           = "sage_agent"
	DefaultPGDatabase       = "postgres"
	DefaultPGSSLMode        = "prefer"
	DefaultPGMaxConnections = 2

	DefaultCollectorInterval   = 60 * time.Second
	DefaultCollectorBatchSize  = 1000
	DefaultCollectorMaxQueries = 500

	DefaultAnalyzerInterval             = 600 * time.Second
	DefaultSlowQueryThresholdMs         = 1000
	DefaultSlowQueryMinCalls            = 5
	DefaultSeqScanMinRows               = 100000
	DefaultUnusedIndexWindowDays        = 7
	DefaultIndexBloatThresholdPct       = 30
	DefaultTableBloatDeadTuplePct       = 20
	DefaultIdleInTxTimeoutMinutes       = 30
	DefaultCacheHitRatioWarning         = 0.95
	DefaultXIDWraparoundWarning         = 500000000
	DefaultXIDWraparoundCritical        = 1000000000
	DefaultRegressionThresholdPct       = 50
	DefaultRegressionLookbackDays       = 7
	DefaultTableBloatMinRows            = 1000
	DefaultAutovacuumTuneMinRows        = 1000000
	DefaultAnalyzeStaleMinRows          = 10000
	DefaultAnalyzeStaleDays             = 7
	DefaultWraparoundFreezeShare        = 0.75
	DefaultCheckpointFreqWarningPerHour = 12

	DefaultCPUCeilingPct            = 90
	DefaultQueryTimeoutMs           = 500
	DefaultDDLTimeoutSeconds        = 300
	DefaultDiskPressureThresholdPct = 5
	DefaultBackoffConsecutiveSkips  = 3
	DefaultDormantIntervalSeconds   = 600

	DefaultTrustLevel            = "observation"
	DefaultTier3Safe             = true
	DefaultTier3Moderate         = false
	DefaultTier3HighRisk         = false
	DefaultRollbackThresholdPct  = 10
	DefaultRollbackWindowMinutes = 15
	DefaultRollbackCooldownDays  = 7
	DefaultCascadeCooldownCycles = 3
	DefaultLockTimeoutMs         = 30000

	// LLM features are on by default (pg_sage is an AI DBA). They reach a
	// provider only once llm.endpoint and llm.api_key are set; until then
	// every feature runs its deterministic path.
	DefaultLLMEnabled         = true
	DefaultLLMTimeoutSeconds  = 30
	DefaultLLMTokenBudget     = 500000
	DefaultLLMContextBudget   = 8192
	DefaultLLMCooldownSeconds = 300

	DefaultIdxOptEnabled            = true
	DefaultIdxOptMinQueryCalls      = 100
	DefaultIdxOptMaxIndexesPerTable = 10
	DefaultIdxOptMaxIncludeColumns  = 3
	DefaultIdxOptWriteHeavyRatio    = 70

	// Optimizer v2 defaults.
	DefaultOptEnabled              = true
	DefaultOptMinQueryCalls        = 100
	DefaultOptMaxIndexesPerTable   = 10
	DefaultOptMaxNewPerTable       = 3
	DefaultOptMaxIncludeColumns    = 3
	DefaultOptWriteHeavyRatioPct   = 70
	DefaultOptMinSnapshots         = 2
	DefaultOptHypoPGMinImprovePct  = 10.0
	DefaultOptConfidenceThreshold  = 0.5
	DefaultOptWriteImpactThreshPct = 15.0

	// Optimizer LLM defaults. The dedicated tier stays opt-in: enabled
	// without its own endpoint it only adds a second, separately budgeted
	// client for the same provider.
	DefaultOptLLMEnabled         = false
	DefaultOptLLMTimeoutSeconds  = 120
	DefaultOptLLMTokenBudget     = 500000
	DefaultOptLLMCooldownSeconds = 300
	DefaultOptLLMMaxOutputTokens = 8192

	DefaultAdvisorEnabled         = true
	DefaultAdvisorIntervalSeconds = 86400

	DefaultBriefingSchedule = "0 6 * * *"

	DefaultRetentionSnapshotsDays = 90
	DefaultRetentionFindingsDays  = 180
	DefaultRetentionActionsDays   = 365
	DefaultRetentionExplainsDays  = 90

	// Notification targets on private networks are refused unless an
	// operator opts in (G7-B21).
	DefaultNotificationPolicyAllowPrivateTargets = false

	DefaultPrometheusListenAddr = "127.0.0.1:8187"

	DefaultRateLimit = 60

	DefaultAPIListenAddr = "0.0.0.0:8080"

	// Alerting defaults.
	DefaultAlertingCheckInterval = 60
	DefaultAlertingCooldown      = 15
	// Minutes between reminders of a still-open finding (alerting-07);
	// 0 = never remind.
	DefaultAlertingRepeatCriticalMinutes = 240
	DefaultAlertingRepeatWarningMinutes  = 1440
	DefaultAlertingRepeatInfoMinutes     = 0

	// Schema lint and DDL-safety polling (the runners' own fallbacks).
	DefaultSchemaLintScanIntervalMinutes = 60
	DefaultMigrationPollIntervalSeconds  = 5

	// auto_explain defaults.
	DefaultAutoExplainLogMinDuration   = 1000
	DefaultAutoExplainCollectInterval  = 300
	DefaultAutoExplainMaxPlansPerCycle = 100

	// Tuner defaults.
	DefaultTunerWorkMemMaxMB        = 512
	DefaultTunerPlanTimeRatio       = 3.0
	DefaultTunerNestedLoopRowThresh = 10000
	DefaultTunerParallelMinRows     = 1000000
	DefaultTunerMinQueryCalls       = 100
	DefaultTunerLLMEnabled          = true

	// Tuner v0.8.5 — Hint revalidation loop (Feature 1).
	DefaultTunerHintRetirementDays        = 14
	DefaultTunerRevalidationIntervalHours = 24
	// DefaultTunerHintAbsentCycles: cycles before an open query_tuning
	// finding of an absent or now-fast statement resolves (tuning-06).
	DefaultTunerHintAbsentCycles = 3
	// DefaultTunerPlanMaxAgeMinutes: age at which a cached plan is stale
	// (tuning-08).
	DefaultTunerPlanMaxAgeMinutes = 1440
	// DefaultTunerParamShapeMinVariants: queryids of one IN-list shape
	// before parameterization advice (tuning-11).
	DefaultTunerParamShapeMinVariants = 20
	// DefaultTunerHintCostTolerancePct: the estimated cost rise a join or
	// scan hint may cause in the hinted EXPLAIN (tuning-05).
	DefaultTunerHintCostTolerancePct = 10

	// Tuner v0.8.5 — Stale-stats detection + ANALYZE (Feature 2).
	DefaultTunerStaleStatsEstimateSkew = 10.0
	DefaultTunerStaleStatsModRatio     = 0.1
	DefaultTunerStaleStatsAgeMinutes   = 60
	DefaultTunerAnalyzeMaxTableMB      = 10240 // 10 GB
	DefaultTunerAnalyzeCooldownMinutes = 60
	DefaultTunerAnalyzeTimeoutMs       = 600000 // 10 minutes
	DefaultTunerMaxConcurrentAnalyze   = 1

	// Analyzer v0.8.5 — work_mem role promotion advisor (Feature 3).
	DefaultAnalyzerWorkMemPromotionThreshold = 5

	// Forecaster defaults.
	DefaultForecasterLookbackDays   = 30
	DefaultForecasterDiskWarnGBDay  = 5.0
	DefaultForecasterConnectionPct  = 80.0
	DefaultForecasterCacheThreshold = 0.95
	// DefaultForecasterCheckpointRequestedPerHour is the requested
	// checkpoint rate that warns (forecast-03).
	DefaultForecasterCheckpointRequestedPerHour = 2.0
	// DefaultForecasterCacheMinBlocksPerDay is the day's block accesses
	// below which its cache hit ratio is left out (forecast-04).
	DefaultForecasterCacheMinBlocksPerDay = 100_000

	// v0.9 — RCA defaults.
	DefaultRCALLMCorrelationThreshold  = 3
	DefaultRCADedupWindowMinutes       = 30
	DefaultRCAEscalationCycles         = 5
	DefaultRCAResolutionCycles         = 2
	DefaultRCAStaleAfterHours          = 24
	DefaultRCAConnectionSaturationPct  = 80
	DefaultRCAReplicationLagThresholdS = 30
	DefaultRCAWALSpikeMultiplier       = 2.0
	DefaultRCALockChainIntervalSeconds = 60
	DefaultRCANarrationEnabled         = true
	// rca-06: critical log lines open incidents within seconds.
	DefaultRCALogFastPathIntervalSeconds = 5
	// rca-07: a recurrence of a log incident within this many hours of
	// its predecessor's resolution is not paged again.
	DefaultRCARecurrenceRenotifyHours = 6

	// v0.9 — Lock chain defaults.
	DefaultLockChainMinBlocked        = 3
	DefaultLockChainCriticalBlocked   = 10
	DefaultLockChainIdleInTxTerminate = 5
	DefaultLockChainActiveQueryCancel = 15

	// v0.9 — Forecaster new defaults.
	DefaultForecasterMinRSquared = 0.5

	// v0.9 — Explain defaults.
	DefaultExplainTimeoutMs       = 10000
	DefaultExplainCacheTTLMinutes = 60
	DefaultExplainMaxTokens       = 4096

	// v0.9.1 — LogWatch defaults.
	DefaultLogWatchPollIntervalMs   = 1000
	DefaultLogWatchDedupWindowS     = 60
	DefaultLogWatchMaxLineLenBytes  = 65536
	DefaultLogWatchTempFileMinBytes = 10485760 // 10MB
	DefaultLogWatchMaxLinesPerCycle = 10000

	DefaultPolicyProfile          = "unattended"
	DefaultToilModelVersion       = 1
	DefaultVerifyWindowMinutes    = 120
	DefaultVerifyWindowMaxMinutes = 4320
	DefaultVerifyMinGainPct       = 20.0
	DefaultVerifyRegressPct       = 15.0
	DefaultVerifyWriteImpactPct   = 20.0
	DefaultVerifyMinSamples       = 30
	DefaultIOBaselineDays         = 7
	DefaultIOSampleRetentionDays  = 14
	DefaultDataIOCeilingPct       = 70
	DefaultWALIOCeilingPct        = 70
	DefaultCloneProvider          = "none"
	DefaultCloneMaxAgeMinutes     = 1440
	// DefaultCloneRehearsalsPerHour is clone.rehearsals_per_hour (lab-04a).
	DefaultCloneRehearsalsPerHour = 2
	// DefaultCloneMaxCreatesPerHour is clone.max_creates_per_hour (lab-05a).
	DefaultCloneMaxCreatesPerHour = 4
	// DefaultCloneRehearsalStatementTimeoutSeconds is
	// clone.rehearsal_statement_timeout_seconds (lab-04a).
	DefaultCloneRehearsalStatementTimeoutSeconds = 5
	// DefaultCloneLakebaseTTLMinutes is clone.lakebase.ttl_minutes: the
	// rehearsal bound (1 h), so a leaked branch lives no longer than one.
	DefaultCloneLakebaseTTLMinutes = 60
	// DefaultCloneLakebaseEndpointMaxCU is clone.lakebase.endpoint_max_cu.
	DefaultCloneLakebaseEndpointMaxCU = 1.0
	// DefaultCloneNeonTTLMinutes is clone.neon.ttl_minutes: the rehearsal
	// bound (1 h), so a leaked branch lives no longer than one.
	DefaultCloneNeonTTLMinutes = 60
	// DefaultCloneNeonEndpointMaxCU is clone.neon.endpoint_max_cu.
	DefaultCloneNeonEndpointMaxCU = 1.0
	// DefaultCloneAuroraTTLMinutes is clone.aurora.ttl_minutes: the
	// rehearsal bound (1 h), clone creation included.
	DefaultCloneAuroraTTLMinutes = 60
	// DefaultCloneRestoreDeadlineMinutes is clone.restore_deadline_minutes:
	// how long a full-copy restore may take to become available.
	DefaultCloneRestoreDeadlineMinutes = 120
	DefaultFreezeRedBufferPct          = 25.0
	DefaultWALAbandonAfterMinutes      = 1440
	DefaultWALRetainedDiskPctCeiling   = 10.0
	DefaultMCPEnabled                  = true
	DefaultMCPTransport                = "http"
	// mcp.call_timeout_seconds: default and accepted range.
	DefaultMCPCallTimeoutSeconds = 90
	MinMCPCallTimeoutSeconds     = 5
	MaxMCPCallTimeoutSeconds     = 600
)
