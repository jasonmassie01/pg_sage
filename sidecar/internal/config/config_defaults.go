package config

import "time"

// newDefaults assembles the built-in defaults, one builder per section.
// testdata/default_config.golden.json pins every field (config-30).
func newDefaults() *Config {
	return &Config{
		Mode:               DefaultMode,
		Postgres:           defaultPostgresConfig(),
		Collector:          defaultCollectorConfig(),
		Analyzer:           defaultAnalyzerConfig(),
		Safety:             defaultSafetyConfig(),
		Trust:              defaultTrustConfig(),
		LLM:                defaultLLMConfig(),
		Advisor:            defaultAdvisorConfig(),
		Briefing:           defaultBriefingConfig(),
		Alerting:           defaultAlertingConfig(),
		SchemaLint:         defaultSchemaLintConfig(),
		Migration:          defaultMigrationConfig(),
		Rehearsal:          RehearsalConfig{PlanFlipPct: DefaultRehearsalPlanFlipPct},
		AutoExplain:        defaultAutoExplainConfig(),
		Forecaster:         defaultForecasterConfig(),
		LogWatch:           defaultLogWatchConfig(),
		Tuner:              defaultTunerConfig(),
		Tuning:             DefaultTuning(),
		RCA:                defaultRCAConfig(),
		SRE:                defaultSREConfig(),
		Specialist:         defaultSpecialistConfig(),
		Runaway:            defaultRunawayConfig(),
		Explain:            defaultExplainConfig(),
		Retention:          defaultRetentionConfig(),
		NotificationPolicy: defaultNotificationPolicyConfig(),
		Prometheus:         defaultPrometheusConfig(),
		API:                defaultAPIConfig(),
		Policy:             PolicyConfig{Profile: DefaultPolicyProfile},
		Value:              ValueConfig{ToilModelVersion: DefaultToilModelVersion},
		Verify:             defaultVerifyConfig(),
		Clone:              defaultCloneConfig(),
		Custodian:          defaultCustodianConfig(),
		MCP:                defaultMCPConfig(),
		Ask:                defaultAskConfig(),
		Facts:              defaultFactsConfig(),
		SelfBudget:         DefaultSelfBudget(),
		Baseline:           DefaultBaseline(),
		History:            DefaultHistory(),
		OAuth:              defaultOAuthConfig(),
		SelfConfig:         defaultSelfConfigConfig(),
		FleetLearning:      defaultFleetLearningConfig(),
		Fleet:              defaultFleetConfig(),
		HA:                 defaultHAConfig(),
		Backup:             defaultBackupConfig(),
		Agents:             defaultAgentsConfig(), Review: defaultReviewConfig(),
		Audit: defaultAuditConfig(), Drill: defaultDrillConfig(),
	}
}

// DefaultConfig returns a Config populated with defaults.
// Exported for use by cmd/gen_config_meta (v0.8.5 Feature 5).
func DefaultConfig() *Config {
	return newDefaults()
}

// defaultPostgresConfig returns the postgres defaults.
func defaultPostgresConfig() PostgresConfig {
	return PostgresConfig{
		Host:           "localhost",
		Port:           DefaultPGPort,
		User:           DefaultPGUser,
		Database:       DefaultPGDatabase,
		SSLMode:        DefaultPGSSLMode,
		MaxConnections: DefaultPGMaxConnections,
	}
}

// defaultCollectorConfig returns the collector defaults.
func defaultCollectorConfig() CollectorConfig {
	return CollectorConfig{
		IntervalSeconds: int(DefaultCollectorInterval / time.Second),
		BatchSize:       DefaultCollectorBatchSize,
		MaxQueries:      DefaultCollectorMaxQueries,
	}
}

// defaultAnalyzerConfig returns the analyzer defaults.
func defaultAnalyzerConfig() AnalyzerConfig {
	return AnalyzerConfig{
		IntervalSeconds:              int(DefaultAnalyzerInterval / time.Second),
		SlowQueryThresholdMs:         DefaultSlowQueryThresholdMs,
		SlowQueryMinCalls:            DefaultSlowQueryMinCalls,
		SeqScanMinRows:               DefaultSeqScanMinRows,
		UnusedIndexWindowDays:        DefaultUnusedIndexWindowDays,
		IndexBloatThresholdPct:       DefaultIndexBloatThresholdPct,
		IndexBloatMinBytes:           DefaultIndexBloatMinBytes,
		TableBloatDeadTuplePct:       DefaultTableBloatDeadTuplePct,
		TableBloatMinRows:            DefaultTableBloatMinRows,
		TableBloatMinBytes:           DefaultTableBloatMinBytes,
		AutovacuumTuneMinRows:        DefaultAutovacuumTuneMinRows,
		AnalyzeStaleMinRows:          DefaultAnalyzeStaleMinRows,
		AnalyzeStaleDays:             DefaultAnalyzeStaleDays,
		WraparoundFreezeShare:        DefaultWraparoundFreezeShare,
		IdleInTxTimeoutMinutes:       DefaultIdleInTxTimeoutMinutes,
		CacheHitRatioWarning:         DefaultCacheHitRatioWarning,
		XIDWraparoundWarning:         DefaultXIDWraparoundWarning,
		XIDWraparoundCritical:        DefaultXIDWraparoundCritical,
		RegressionThresholdPct:       DefaultRegressionThresholdPct,
		RegressionLookbackDays:       DefaultRegressionLookbackDays,
		CheckpointFreqWarningPerHour: DefaultCheckpointFreqWarningPerHour,
		WorkMemPromotionThreshold:    DefaultAnalyzerWorkMemPromotionThreshold,

		SchemaGuardDDLDebounceSeconds: DefaultSchemaGuardDDLDebounceSeconds,
		SelfCostBudgetMs:              DefaultSelfCostBudgetMs,
		InactiveSlotMinMinutes:        DefaultInactiveSlotMinMinutes,
		SubscriptionLagMinutes:        DefaultSubscriptionLagMinutes,
		CDCStallMinutes:               DefaultCDCStallMinutes,
		LockChain: LockChainConfig{
			Enabled:                  true,
			MinBlockedThreshold:      DefaultLockChainMinBlocked,
			CriticalBlockedThreshold: DefaultLockChainCriticalBlocked,
			IdleInTxTerminateMinutes: DefaultLockChainIdleInTxTerminate,
			ActiveQueryCancelMinutes: DefaultLockChainActiveQueryCancel,
			SafePatterns:             []string{"pg_sage", "replication", "patroni"},
		},
	}
}

// defaultSafetyConfig returns the safety defaults.
func defaultSafetyConfig() SafetyConfig {
	return SafetyConfig{
		CPUCeilingPct:            DefaultCPUCeilingPct,
		DataIOCeilingPct:         DefaultDataIOCeilingPct,
		WALIOCeilingPct:          DefaultWALIOCeilingPct,
		QueryTimeoutMs:           DefaultQueryTimeoutMs,
		DDLTimeoutSeconds:        DefaultDDLTimeoutSeconds,
		DiskPressureThresholdPct: DefaultDiskPressureThresholdPct,
		BackoffConsecutiveSkips:  DefaultBackoffConsecutiveSkips,
		DormantIntervalSeconds:   DefaultDormantIntervalSeconds,
		LockTimeoutMs:            DefaultLockTimeoutMs,
		IndexBuildTimeoutSeconds: DefaultClassTimeoutSeconds,
		ReindexTimeoutSeconds:    DefaultClassTimeoutSeconds,
		VacuumTimeoutSeconds:     DefaultClassTimeoutSeconds,
		VacuumStallMinutes:       DefaultVacuumStallMinutes,
	}
}

// defaultTrustConfig returns the trust defaults.
func defaultTrustConfig() TrustConfig {
	return TrustConfig{
		Level:                 DefaultTrustLevel,
		Tier3Safe:             DefaultTier3Safe,
		Tier3Moderate:         DefaultTier3Moderate,
		Tier3HighRisk:         DefaultTier3HighRisk,
		RampSafeHours:         DefaultRampSafeHours,
		RampModerateHours:     DefaultRampModerateHours,
		AskAfterApprovals:     DefaultAskAfterApprovals,
		RollbackThresholdPct:  DefaultRollbackThresholdPct,
		RollbackWindowMinutes: DefaultRollbackWindowMinutes,
		RollbackCooldownDays:  DefaultRollbackCooldownDays,
		CascadeCooldownCycles: DefaultCascadeCooldownCycles,
	}
}

// defaultLLMConfig returns the LLM defaults.
func defaultLLMConfig() LLMConfig {
	return LLMConfig{
		Enabled:               DefaultLLMEnabled,
		TimeoutSeconds:        DefaultLLMTimeoutSeconds,
		TokenBudgetDaily:      DefaultLLMTokenBudget,
		ContextBudgetTokens:   DefaultLLMContextBudget,
		CooldownSeconds:       DefaultLLMCooldownSeconds,
		TokenParameter:        DefaultLLMTokenParameter,
		ToolReasoningEffort:   DefaultLLMToolReasoningEffort,
		RedactNumericLiterals: DefaultLLMRedactNumericLiterals,
		IndexOptimizer: IndexOptimizerConfig{
			Enabled:            DefaultIdxOptEnabled,
			MinQueryCalls:      DefaultIdxOptMinQueryCalls,
			MaxIndexesPerTable: DefaultIdxOptMaxIndexesPerTable,
			MaxIncludeColumns:  DefaultIdxOptMaxIncludeColumns,
			WriteHeavyRatio:    DefaultIdxOptWriteHeavyRatio,
		},
		Optimizer: OptimizerConfig{
			Enabled:              DefaultOptEnabled,
			MinQueryCalls:        DefaultOptMinQueryCalls,
			MaxIndexesPerTable:   DefaultOptMaxIndexesPerTable,
			MaxNewPerTable:       DefaultOptMaxNewPerTable,
			MaxIncludeColumns:    DefaultOptMaxIncludeColumns,
			WriteHeavyRatioPct:   DefaultOptWriteHeavyRatioPct,
			MinSnapshots:         DefaultOptMinSnapshots,
			HypoPGMinImprovePct:  DefaultOptHypoPGMinImprovePct,
			ConfidenceThreshold:  DefaultOptConfidenceThreshold,
			WriteImpactThreshPct: DefaultOptWriteImpactThreshPct,
			RejectionMemory:      DefaultOptimizerRejectionMemory(),
		},
		OptimizerLLM: OptimizerLLMConfig{
			Enabled:           DefaultOptLLMEnabled,
			TimeoutSeconds:    DefaultOptLLMTimeoutSeconds,
			TokenBudgetDaily:  DefaultOptLLMTokenBudget,
			CooldownSeconds:   DefaultOptLLMCooldownSeconds,
			MaxOutputTokens:   DefaultOptLLMMaxOutputTokens,
			FallbackToGeneral: true,
		},
	}
}

// defaultAdvisorConfig returns the advisor defaults.
func defaultAdvisorConfig() AdvisorConfig {
	return AdvisorConfig{
		Enabled:           DefaultAdvisorEnabled,
		IntervalSeconds:   DefaultAdvisorIntervalSeconds,
		VacuumEnabled:     true,
		WALEnabled:        true,
		ConnectionEnabled: true,
		MemoryEnabled:     true,
		BudgetProposals:   true,
		RewriteEnabled:    true,
		BloatEnabled:      true,
	}
}

// defaultBriefingConfig returns the briefing defaults.
func defaultBriefingConfig() BriefingConfig {
	return BriefingConfig{
		Schedule:  DefaultBriefingSchedule,
		Channels:  []string{"stdout"},
		FleetMode: BriefingFleetRollup,
	}
}

// defaultAlertingConfig returns the alerting defaults.
func defaultAlertingConfig() AlertingConfig {
	return AlertingConfig{
		Enabled:              false,
		Engine:               AlertingEngineNotify,
		CheckIntervalSeconds: DefaultAlertingCheckInterval,
		CooldownMinutes:      DefaultAlertingCooldown,
		RepeatMinutes:        ResolveAlertRepeatMinutes(nil),
		Timezone:             "UTC",
	}
}

// defaultSchemaLintConfig returns the schema_lint defaults.
func defaultSchemaLintConfig() SchemaLintConfig {
	return SchemaLintConfig{
		ScanIntervalMinutes: DefaultSchemaLintScanIntervalMinutes,
	}
}

// defaultMigrationConfig returns the migration defaults.
func defaultMigrationConfig() MigrationConfig {
	return MigrationConfig{
		PollIntervalSeconds:  DefaultMigrationPollIntervalSeconds,
		ContractDelayMinutes: DefaultMigrationContractDelayMinutes,
		MaxLockMS:            DefaultMigrationMaxLockMS,
	}
}

// defaultAutoExplainConfig returns the auto_explain defaults.
func defaultAutoExplainConfig() AutoExplainConfig {
	return AutoExplainConfig{
		Enabled:                true,
		LogMinDurationMs:       DefaultAutoExplainLogMinDuration,
		CollectIntervalSeconds: DefaultAutoExplainCollectInterval,
		MaxPlansPerCycle:       DefaultAutoExplainMaxPlansPerCycle,
	}
}

// defaultForecasterConfig returns the forecaster defaults.
func defaultForecasterConfig() ForecasterConfig {
	return ForecasterConfig{
		Enabled:                    true,
		LookbackDays:               DefaultForecasterLookbackDays,
		DiskWarnGrowthGBDay:        DefaultForecasterDiskWarnGBDay,
		ConnectionWarnPct:          DefaultForecasterConnectionPct,
		CacheWarnThreshold:         DefaultForecasterCacheThreshold,
		CacheMinBlocksPerDay:       DefaultForecasterCacheMinBlocksPerDay,
		CheckpointRequestedPerHour: DefaultForecasterCheckpointRequestedPerHour,
		DiskCapacityBytes:          0,
		MinRSquared:                DefaultForecasterMinRSquared,
	}
}

// defaultLogWatchConfig returns the log_watch defaults. They were once
// absent from newDefaults, so with logwatch enabled but no explicit values, MaxLinesPerCycle and
// TempFileMinBytes defaulted to 0 — unbounded parsing and a
// defeated 10MB temp-file floor (W1, default-value masking).
func defaultLogWatchConfig() LogWatchConfig {
	return LogWatchConfig{
		PollIntervalMs:   DefaultLogWatchPollIntervalMs,
		DedupWindowS:     DefaultLogWatchDedupWindowS,
		MaxLineLenBytes:  DefaultLogWatchMaxLineLenBytes,
		TempFileMinBytes: DefaultLogWatchTempFileMinBytes,
		MaxLinesPerCycle: DefaultLogWatchMaxLinesPerCycle,
	}
}

// defaultTunerConfig returns the tuner defaults.
func defaultTunerConfig() TunerConfig {
	return TunerConfig{
		Enabled:                true,
		WorkMemMaxMB:           DefaultTunerWorkMemMaxMB,
		PlanTimeRatio:          DefaultTunerPlanTimeRatio,
		NestedLoopRowThreshold: DefaultTunerNestedLoopRowThresh,
		ParallelMinTableRows:   DefaultTunerParallelMinRows,
		MinQueryCalls:          DefaultTunerMinQueryCalls,
		LLMEnabled:             DefaultTunerLLMEnabled,
		VerifyAfterApply:       true,

		// Feature 1 — Hint revalidation loop.
		HintRetirementDays:        DefaultTunerHintRetirementDays,
		RevalidationIntervalHours: DefaultTunerRevalidationIntervalHours,
		HintAbsentCycles:          DefaultTunerHintAbsentCycles,
		PlanMaxAgeMinutes:         DefaultTunerPlanMaxAgeMinutes,
		ParamShapeMinVariants:     DefaultTunerParamShapeMinVariants,
		HintCostTolerancePct:      DefaultTunerHintCostTolerancePct,

		// Feature 2 — Stale-stats detection + ANALYZE.
		StaleStatsEstimateSkew: DefaultTunerStaleStatsEstimateSkew,
		StaleStatsModRatio:     DefaultTunerStaleStatsModRatio,
		StaleStatsAgeMinutes:   DefaultTunerStaleStatsAgeMinutes,
		AnalyzeMaxTableMB:      DefaultTunerAnalyzeMaxTableMB,
		AnalyzeCooldownMinutes: DefaultTunerAnalyzeCooldownMinutes,
		AnalyzeTimeoutMs:       DefaultTunerAnalyzeTimeoutMs,
		MaxConcurrentAnalyze:   DefaultTunerMaxConcurrentAnalyze,
	}
}

// defaultRCAConfig returns the RCA defaults.
func defaultRCAConfig() RCAConfig {
	return RCAConfig{
		Enabled:                    true,
		LLMCorrelationThreshold:    DefaultRCALLMCorrelationThreshold,
		DedupWindowMinutes:         DefaultRCADedupWindowMinutes,
		EscalationCycles:           DefaultRCAEscalationCycles,
		ResolutionCycles:           DefaultRCAResolutionCycles,
		StaleAfterHours:            DefaultRCAStaleAfterHours,
		VacuumMinDeadTuples:        DefaultRCAVacuumMinDeadTuples,
		VacuumMinTableMB:           DefaultRCAVacuumMinTableMB,
		XminHolderMinMinutes:       DefaultRCAXminHolderMinMinutes,
		PreparedXactMinAgeMinutes:  DefaultRCAPreparedXactMinAgeMinutes,
		ConnectionSaturationPct:    DefaultRCAConnectionSaturationPct,
		ReplicationLagThresholdS:   DefaultRCAReplicationLagThresholdS,
		WALSpikeMultiplier:         DefaultRCAWALSpikeMultiplier,
		LockChainIntervalSeconds:   DefaultRCALockChainIntervalSeconds,
		NarrationEnabled:           DefaultRCANarrationEnabled,
		LogFastPathIntervalSeconds: DefaultRCALogFastPathIntervalSeconds,
		RecurrenceRenotifyHours:    DefaultRCARecurrenceRenotifyHours,
		DeadlockStatementText:      DefaultRCADeadlockStatementText,
	}
}

// defaultRunawayConfig returns the runaway defaults.
func defaultRunawayConfig() RunawayConfig {
	return RunawayConfig{
		Enabled: false,
		Policies: []RunawayPolicy{
			{Name: "long_running", MaxDurationMinutes: 30, WarnCycles: 2, CancelCycles: 4},
			{Name: "blocker", MaxBlockedSessions: 5, WarnCycles: 1, CancelCycles: 2},
		},
		SafePatterns: []string{"pg_dump", "pg_basebackup"},
	}
}

// defaultExplainConfig returns the explain defaults.
func defaultExplainConfig() ExplainConfig {
	return ExplainConfig{
		Enabled:         true,
		TimeoutMs:       DefaultExplainTimeoutMs,
		CacheTTLMinutes: DefaultExplainCacheTTLMinutes,
		MaxTokens:       DefaultExplainMaxTokens,
	}
}

// defaultRetentionConfig returns the retention defaults.
func defaultRetentionConfig() RetentionConfig {
	return RetentionConfig{
		SnapshotsDays: DefaultRetentionSnapshotsDays,
		FindingsDays:  DefaultRetentionFindingsDays,
		ActionsDays:   DefaultRetentionActionsDays,
		ExplainsDays:  DefaultRetentionExplainsDays,

		SageSizeWarningPct: DefaultRetentionSageSizeWarningPct,
		DecisionsDays:      DefaultRetentionDecisionsDays,
		AuthAuditDays:      DefaultRetentionAuthAuditDays,
		QueryStoreDays:     DefaultRetentionQueryStoreDays,
		SnapshotsMaxPct:    DefaultRetentionSnapshotsMaxPct,

		QueryRollupHourlyDays: DefaultRetentionQueryRollupHourlyDays,
		QueryRollupDailyDays:  DefaultRetentionQueryRollupDailyDays,
		WorkloadPacksDays:     DefaultRetentionWorkloadPacksDays,
	}
}

// defaultNotificationPolicyConfig returns the notification_policy defaults.
func defaultNotificationPolicyConfig() NotificationPolicyConfig {
	return NotificationPolicyConfig{
		AllowPrivateTargets: DefaultNotificationPolicyAllowPrivateTargets,
	}
}

// defaultPrometheusConfig returns the prometheus defaults.
func defaultPrometheusConfig() PrometheusConfig {
	return PrometheusConfig{
		ListenAddr: DefaultPrometheusListenAddr,
	}
}

// defaultAPIConfig returns the API defaults.
func defaultAPIConfig() APIConfig {
	return APIConfig{
		ListenAddr: DefaultAPIListenAddr,
	}
}

// defaultVerifyConfig returns the verify defaults.
func defaultVerifyConfig() VerifyConfig {
	return VerifyConfig{
		WindowMinutes:             DefaultVerifyWindowMinutes,
		WindowMaxMinutes:          DefaultVerifyWindowMaxMinutes,
		MinGainPct:                DefaultVerifyMinGainPct,
		RegressPct:                DefaultVerifyRegressPct,
		WriteImpactPct:            DefaultVerifyWriteImpactPct,
		MinSamples:                DefaultVerifyMinSamples,
		DropWindowHours:           DefaultVerifyDropWindowHours,
		Method:                    DefaultVerifyMethod,
		OnExpired:                 DefaultVerifyOnExpired,
		BacktestMaxRuntimeMinutes: DefaultVerifyBacktestMaxRuntimeMinutes,
		BacktestPlacebos:          DefaultVerifyBacktestPlacebos,
		IOBaselineDays:            DefaultIOBaselineDays,
		IOSampleDays:              DefaultIOSampleRetentionDays,
	}
}

// defaultCloneConfig returns the clone defaults.
func defaultCloneConfig() CloneProviderConfig {
	return CloneProviderConfig{
		Provider:                         DefaultCloneProvider,
		MaxCloneAgeMinutes:               DefaultCloneMaxAgeMinutes,
		RehearsalsPerHour:                DefaultCloneRehearsalsPerHour,
		MaxCreatesPerHour:                DefaultCloneMaxCreatesPerHour,
		RehearsalStatementTimeoutSeconds: DefaultCloneRehearsalStatementTimeoutSeconds,
		Lakebase: CloneLakebaseConfig{TTLMinutes: DefaultCloneLakebaseTTLMinutes,
			EndpointMaxCU: DefaultCloneLakebaseEndpointMaxCU},
		Neon: CloneNeonConfig{TTLMinutes: DefaultCloneNeonTTLMinutes,
			EndpointMaxCU: DefaultCloneNeonEndpointMaxCU},
		Aurora:                 CloneAuroraConfig{TTLMinutes: DefaultCloneAuroraTTLMinutes},
		RestoreDeadlineMinutes: DefaultCloneRestoreDeadlineMinutes,
	}
}

// defaultCustodianConfig returns the custodian defaults.
func defaultCustodianConfig() CustodianConfig {
	return CustodianConfig{
		Freeze: FreezeCustodianConfig{RedBufferPct: DefaultFreezeRedBufferPct},
		WAL: WALCustodianConfig{
			AbandonAfterMinutes:       DefaultWALAbandonAfterMinutes,
			RetainedWALDiskPctCeiling: DefaultWALRetainedDiskPctCeiling,
		},
	}
}

// defaultMCPConfig returns the MCP server defaults.
func defaultMCPConfig() MCPConfig {
	return MCPConfig{Enabled: DefaultMCPEnabled, Transport: DefaultMCPTransport,
		OAuth:              defaultMCPOAuthConfig(),
		CallTimeoutSeconds: DefaultMCPCallTimeoutSeconds}
}

// defaultOAuthConfig returns the oauth defaults.
func defaultOAuthConfig() OAuthConfig {
	return OAuthConfig{
		DefaultRole:   "viewer",
		GroupsClaim:   "groups",
		UnmappedUsers: UnmappedUsersDeny,
	}
}
