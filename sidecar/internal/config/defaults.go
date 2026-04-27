package config

import "time"

// Default values matching the spec.
const (
	DefaultMode = "extension"

	DefaultPGPort           = 5432
	DefaultPGUser           = "sage_agent"
	DefaultPGDatabase       = "postgres"
	DefaultPGSSLMode        = "prefer"
	DefaultPGMaxConnections = 2

	DefaultCollectorInterval   = 60 * time.Second
	DefaultCollectorBatchSize  = 1000
	DefaultCollectorMaxQueries = 500

	DefaultAnalyzerInterval              = 600 * time.Second
	DefaultSlowQueryThresholdMs          = 1000
	DefaultSeqScanMinRows                = 100000
	DefaultUnusedIndexWindowDays         = 7
	DefaultIndexBloatThresholdPct        = 30
	DefaultTableBloatDeadTuplePct        = 20
	DefaultIdleInTxTimeoutMinutes        = 30
	DefaultCacheHitRatioWarning          = 0.95
	DefaultXIDWraparoundWarning          = 500000000
	DefaultXIDWraparoundCritical         = 1000000000
	DefaultRegressionThresholdPct        = 50
	DefaultRegressionLookbackDays        = 7
	DefaultTableBloatMinRows             = 1000
	DefaultCheckpointFreqWarningPerHour  = 12

	DefaultCPUCeilingPct             = 90
	DefaultQueryTimeoutMs            = 500
	DefaultDDLTimeoutSeconds         = 300
	DefaultDiskPressureThresholdPct  = 5
	DefaultBackoffConsecutiveSkips   = 3
	DefaultDormantIntervalSeconds    = 600

	DefaultTrustLevel          = "observation"
	DefaultTier3Safe           = true
	DefaultTier3Moderate       = false
	DefaultTier3HighRisk       = false
	DefaultRollbackThresholdPct  = 10
	DefaultRollbackWindowMinutes = 15
	DefaultRollbackCooldownDays    = 7
	DefaultCascadeCooldownCycles   = 3
	DefaultLockTimeoutMs           = 30000

	DefaultLLMEnabled        = false
	DefaultLLMTimeoutSeconds = 30
	DefaultLLMTokenBudget    = 500000
	DefaultLLMContextBudget  = 8192
	DefaultLLMCooldownSeconds = 300

	DefaultIdxOptEnabled           = false
	DefaultIdxOptMinQueryCalls     = 100
	DefaultIdxOptMaxIndexesPerTable = 10
	DefaultIdxOptMaxIncludeColumns = 3
	DefaultIdxOptOverIndexedRatio  = 150
	DefaultIdxOptWriteHeavyRatio   = 70

	// Optimizer v2 defaults.
	DefaultOptEnabled              = false
	DefaultOptMinQueryCalls        = 100
	DefaultOptMaxIndexesPerTable   = 10
	DefaultOptMaxNewPerTable       = 3
	DefaultOptMaxIncludeColumns    = 3
	DefaultOptOverIndexedRatioPct  = 150
	DefaultOptWriteHeavyRatioPct   = 70
	DefaultOptMinSnapshots         = 2
	DefaultOptHypoPGMinImprovePct  = 10.0
	DefaultOptPlanSource           = "auto"
	DefaultOptConfidenceThreshold  = 0.5
	DefaultOptWriteImpactThreshPct = 15.0

	// Optimizer LLM defaults.
	DefaultOptLLMTimeoutSeconds  = 120
	DefaultOptLLMTokenBudget     = 500000
	DefaultOptLLMCooldownSeconds = 300
	DefaultOptLLMMaxOutputTokens = 8192

	DefaultBriefingSchedule = "0 6 * * *"

	DefaultRetentionSnapshotsDays = 90
	DefaultRetentionFindingsDays  = 180
	DefaultRetentionActionsDays   = 365
	DefaultRetentionExplainsDays  = 90

	DefaultPrometheusListenAddr = "127.0.0.1:9187"

	DefaultRateLimit = 60

	DefaultAPIListenAddr = "0.0.0.0:8080"

	// Alerting defaults.
	DefaultAlertingCheckInterval = 60
	DefaultAlertingCooldown      = 15

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

	// Tuner v0.8.5 — Hint revalidation loop (Feature 1).
	DefaultTunerHintRetirementDays           = 14
	DefaultTunerRevalidationIntervalHours    = 24
	DefaultTunerRevalidationKeepRatio        = 1.2
	DefaultTunerRevalidationRollbackRatio    = 0.8
	DefaultTunerRevalidationExplainTimeoutMs = 10000

	// Tuner v0.8.5 — Stale-stats detection + ANALYZE (Feature 2).
	DefaultTunerStaleStatsEstimateSkew        = 10.0
	DefaultTunerStaleStatsModRatio            = 0.1
	DefaultTunerStaleStatsAgeMinutes          = 60
	DefaultTunerAnalyzeMaxTableMB             = 10240 // 10 GB
	DefaultTunerAnalyzeCooldownMinutes        = 60
	DefaultTunerAnalyzeMaintenanceThresholdMB = 1024 // 1 GB
	DefaultTunerAnalyzeTimeoutMs              = 600000 // 10 minutes
	DefaultTunerMaxConcurrentAnalyze          = 1

	// Analyzer v0.8.5 — work_mem role promotion advisor (Feature 3).
	DefaultAnalyzerWorkMemPromotionThreshold = 5

	// Forecaster defaults.
	DefaultForecasterLookbackDays  = 30
	DefaultForecasterDiskWarnGBDay = 5.0
	DefaultForecasterConnectionPct = 80.0
	DefaultForecasterCacheThreshold = 0.95
	DefaultForecasterSeqWarnDays   = 90
	DefaultForecasterSeqCritDays   = 30

	// v0.9 — RCA defaults.
	DefaultRCALLMCorrelationThreshold  = 3
	DefaultRCADedupWindowMinutes       = 30
	DefaultRCAEscalationCycles         = 5
	DefaultRCAResolutionCycles         = 2
	DefaultRCAConnectionSaturationPct  = 80
	DefaultRCAReplicationLagThresholdS = 30
	DefaultRCAWALSpikeMultiplier       = 2.0

	// v0.9 — Lock chain defaults.
	DefaultLockChainMinBlocked        = 3
	DefaultLockChainCriticalBlocked   = 10
	DefaultLockChainIdleInTxTerminate = 5
	DefaultLockChainActiveQueryCancel = 15

	// v0.9 — Forecaster new defaults.
	DefaultForecasterMinDataPoints = 24
	DefaultForecasterMinRSquared   = 0.5

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
)
