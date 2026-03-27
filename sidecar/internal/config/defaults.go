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

	DefaultCollectorInterval = 60 * time.Second
	DefaultCollectorBatchSize = 1000

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
	DefaultRollbackCooldownDays  = 7

	DefaultLLMEnabled        = false
	DefaultLLMTimeoutSeconds = 30
	DefaultLLMTokenBudget    = 50000
	DefaultLLMContextBudget  = 4096
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
	DefaultOptLLMTokenBudget     = 50000
	DefaultOptLLMCooldownSeconds = 300
	DefaultOptLLMMaxOutputTokens = 4096

	DefaultBriefingSchedule = "0 6 * * *"

	DefaultRetentionSnapshotsDays = 90
	DefaultRetentionFindingsDays  = 180
	DefaultRetentionActionsDays   = 365
	DefaultRetentionExplainsDays  = 90

	DefaultMCPListenAddr        = "0.0.0.0:5433"
	DefaultPrometheusListenAddr = "0.0.0.0:9187"

	DefaultRateLimit = 60

	DefaultAPIListenAddr = "0.0.0.0:8080"
)
