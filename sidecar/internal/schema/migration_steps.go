package schema

// migrationStep is one ordered group of idempotent DDL run by
// runMigrationSteps (config-04). name is the constant's name, used in logs
// and errors. alwaysRun marks a step with a statement the precheck
// classifier does not know: it was reviewed and runs on every boot.
type migrationStep struct {
	name      string
	sql       string
	alwaysRun bool
}

// migrationStatements is the SQL of migrationSteps, in order.
func migrationStatements() []string {
	steps := migrationSteps()
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.sql)
	}
	return out
}

// migrationSteps lists the idempotent schema changes for existing installs,
// in the order they run. Append new steps at the end.
func migrationSteps() []migrationStep {
	steps := []migrationStep{
		{name: "ddlActionLogApprovalCols", sql: ddlActionLogApprovalCols},
		{name: "ddlUsersOAuth", sql: ddlUsersOAuth},
		{name: "ddlAuthAccountLinking", sql: ddlAuthAccountLinking},
		{name: "ddlQueryHintsRewrite", sql: ddlQueryHintsRewrite},
		{name: "ddlQueryHintsRevalidate", sql: ddlQueryHintsRevalidate},
		{name: "ddlIncidentsLastDetected", sql: ddlIncidentsLastDetected},
		{name: "ddlActionQueueLifecycleCols", sql: ddlActionQueueLifecycleCols},
		{name: "ddlSchemaFindingsLintRunner", sql: ddlSchemaFindingsLintRunner},
		{name: "ddlFindingsAbsorbsSchemaFindings", sql: ddlFindingsAbsorbsSchemaFindings},
		{name: "ddlFindingsBackfillFromSchemaFindings",
			sql: ddlFindingsBackfillFromSchemaFindings},
		{name: "ddlFleetScaleIndexes", sql: ddlFleetScaleIndexes},
		{name: "ddlQueryStoreStatsEpoch", sql: ddlQueryStoreStatsEpoch},
		{name: "ddlExplainCachePlanHash", sql: ddlExplainCachePlanHash},
		{name: "ddlExplainCacheProvenance", sql: ddlExplainCacheProvenance},
	}
	for _, m := range agentNativeMigrations() {
		steps = append(steps, migrationStep{name: "agentNative." + m.Name, sql: m.SQL})
	}
	// After the agent-native DDL: sage.policy and sage.table_contract must
	// exist. The window migration (policy schema version 3) follows the
	// class split (version 2).
	steps = append(steps, namedSteps(laterMigrations())...)
	steps = append(steps, namedSteps(latestMigrations())...)
	steps = append(steps, namedSteps(trancheOneMigrations())...)
	steps = append(steps, namedSteps(trancheTwoMigrations())...)
	steps = append(steps, namedSteps(trancheThreeMigrations())...)
	steps = append(steps, namedSteps(trancheFourMigrations())...)
	return append(steps, namedSteps(trancheFiveMigrations())...)
}

// namedStep pairs a constant's name with its SQL for laterMigrations.
type namedStep = [2]string

func namedSteps(list []namedStep) []migrationStep {
	out := make([]migrationStep, 0, len(list))
	for _, s := range list {
		out = append(out, migrationStep{name: s[0], sql: s[1]})
	}
	return out
}

// laterMigrations are the steps after the agent-native DDL, in order.
func laterMigrations() []namedStep {
	return []namedStep{
		{"ddlPolicyChangeClassSplit", ddlPolicyChangeClassSplit},
		{"ddlPolicyWindowCronDuration", ddlPolicyWindowCronDuration},
		{"ddlRetentionColumnDeclaration", ddlRetentionColumnDeclaration},
		{"ddlTableContractIdentity", ddlTableContractIdentity},
		{"ddlSRECoordination", ddlSRECoordination},
		{"ddlSREInvestigator", ddlSREInvestigator},
		{"ddlSREModelEvents", ddlSREModelEvents},
		{"ddlSREReasoningBudget", ddlSREReasoningBudget},
		{"ddlSRESLOChangeEvents", ddlSRESLOChangeEvents},
		{"ddlSREActions", ddlSREActions},
		{"ddlRecommendationAll", ddlRecommendationAll},
		{"ddlRunwaySamples", ddlRunwaySamples},
		{"ddlSRERunbooks", ddlSRERunbooks},
		{"ddlSREAutonomy", ddlSREAutonomy},
		{"ddlSREAutonomyCarryOver", ddlSREAutonomyCarryOver},
		{"ddlSREAutonomyDatabaseScope", ddlSREAutonomyDatabaseScope},
		{"ddlSREBenchProvenance", ddlSREBenchProvenance},
		{"ddlSREModelLift", ddlSREModelLift},
		{"ddlHAIdentity", ddlHAIdentity},
		{"ddlDebtExec", ddlDebtExec},
		{"ddlIncidentOpenIdentity", ddlIncidentOpenIdentity},
		{"ddlSnapshotDelta", ddlSnapshotDelta},
		{"ddlDecisionLedger", ddlDecisionLedger()},
		{"ddlPerfIndexes", ddlPerfIndexes},
		{"ddlSREPerf", ddlSREPerf()},
		{"ddlAPIListIndexes", ddlAPIListIndexes()},
		{"ddlSelfExclIndexes", ddlSelfExclIndexes()},
		{"ddlGuardHistoryIndexes", ddlGuardHistoryIndexes()},
	}
}

// latestMigrations are the steps after laterMigrations, in order.
func latestMigrations() []namedStep {
	return []namedStep{
		{"ddlApprovalCards", ddlApprovalCards},
		{"ddlOptimizerRejection", ddlOptimizerRejection},
		{"ddlActionOutcome", ddlActionOutcome()},
		{"ddlTrustLedger", ddlTrustLedger()},
		{"ddlVerificationWait", ddlVerificationWait()},
		{"ddlShadowMode", ddlShadowMode()},
		{"ddlFacts", ddlFacts},
		{"ddlSREAutonomyRootAuthority", ddlSREAutonomyRootAuthority},
		{"ddlMCPv2", ddlMCPv2},
		{"ddlObserverIndexes", ddlObserverIndexes},
		{"ddlSelfConfig", ddlSelfConfig},
		{"ddlTuningBudgetDay", ddlTuningBudgetDay},
		{"ddlOnboarding", ddlOnboarding},
		{"ddlSpecialist", ddlSpecialist},
		{"ddlSpecialistQueryScope", ddlSpecialistQueryScope},
		{"ddlAsk", ddlAsk},
		{"ddlManagedChangeProposals", ddlManagedChangeProposals},
		{"ddlFleetLearning", ddlFleetLearning},
		{"ddlIndexReplace", ddlIndexReplace},
		{"ddlChangeFeedIndexes", ddlChangeFeedIndexes()},
		{"ddlDecommission", ddlDecommission},
		{"ddlAuthAuditSourceIP", ddlAuthAuditSourceIP},
		{"ddlAuthAuditCreatedIndex", ddlAuthAuditCreatedIndex},
		{"ddlGuardCore", ddlGuardCore}, {"ddlGuardCoreValidate", ddlGuardCoreValidate},
		{"ddlAgentEnvClass", ddlAgentEnvClass}, {"ddlAgentGate", ddlAgentGate},
		{"ddlGuardKill", ddlGuardKill}, {"ddlGuardQueryAudit", ddlGuardQueryAudit},
		{"ddlGuardGrants", ddlGuardGrants}, {"ddlGuardOps", ddlGuardOps},
		// E2: after the core principal tables, which the bindings reference.
		{"ddlGuardPGAuditEvents", ddlGuardPGAuditEvents},
		{"ddlGuardIdentityBindings", ddlGuardIdentityBindings},
		{"ddlSIEMCursor", ddlSIEMCursor},
		{"ddlQueryHintsLifecycle", ddlQueryHintsLifecycle}, // tuning-01
		{"ddlVerificationObservationRetry", ddlVerificationObservationRetry},
		{"ddlEmergencyStopFunction", ddlEmergencyStopFunction},
		{"ddlEmergencyStopSeed", ddlEmergencyStopSeed},
		// clouds-02: after the table (ddlManagedChangeProposals).
		{"ddlManagedChangeFingerprintV2", ddlManagedChangeFingerprintV2},
		{"ddlSessionsTokenHash", ddlSessionsTokenHash}, // apiauth-04: hashed session cookies
		{"ddlUsersDisabledAt", ddlUsersDisabledAt},     // apiauth-01: user deactivation
	}
}

// trancheOneMigrations are the steps after latestMigrations, in order.
func trancheOneMigrations() []namedStep {
	return []namedStep{
		{"ddlUnusedIndexClock", ddlUnusedIndexClock},                     // rules-04
		{"ddlSelfConfigDerivations", ddlSelfConfigDerivations},           // footprint-04
		{"ddlPreTaggingSelfFindings", ddlPreTaggingSelfFindings},         // footprint-06
		{"ddlQueryHintsStatusCheck", ddlQueryHintsStatusCheck},           // tuning-06
		{"ddlAdvisorRuns", ddlAdvisorRuns},                               // advisors-02
		{"ddlRolloutInstanceStatus", ddlRolloutInstanceStatus},           // executor-04
		{"ddlPGAuditActionTagIndex", ddlPGAuditActionTagIndex},           // audit-06
		{"ddlApprovalCardNextCheck", ddlApprovalCardNextCheck},           // policy-06
		{"ddlActionQueueRevertsAction", ddlActionQueueRevertsAction},     // policy-05
		{"ddlDecisionCandidateKey", ddlDecisionCandidateKey()},           // policy-02
		{"ddlDecisionPolicyProvenance", ddlDecisionPolicyProvenance},     // policy-03
		{"ddlSREAttempts", ddlSREAttempts},                               // sre-01
		{"ddlFactsMissingSince", ddlFactsMissingSince},                   // facts-08
		{"ddlFactEvents", ddlFactEvents},                                 // facts-07
		{"ddlMigrationRunBlocked", ddlMigrationRunBlocked},               // migration-03
		{"ddlLLMUsage", ddlLLMUsage},                                     // fleet-10
		{"ddlFleetLeaseControl", ddlFleetLeaseControl},                   // fleet-03: control database on the lease
		{"ddlApprovalFollowupClaim", ddlApprovalFollowupClaim},           // fleet-03: fenced follow-up claims
		{"ddlMCPTokensMetricsScope", ddlMCPTokensMetricsScope},           // observability-08: scrape tokens
		{"ddlUsersDatabricksPin", ddlUsersDatabricksPin},                 // databricks-07: Apps forwarded-user pin
		{"ddlTrustLedgerIncidentCursors", ddlTrustLedgerIncidentCursors}, // earned-02: after ddlTrustLedger()
		{"ddlIncidentAvoidedRevert", ddlIncidentAvoidedRevert},           // earned-09: after ddlAgentNativeValue
		{"ddlBenchCrashRecovery", ddlBenchCrashRecovery},                 // benches-03: after ddlSREAutonomy (sre_game_days)
	}
}

// trancheTwoMigrations are the T2 steps, in landing order.
func trancheTwoMigrations() []namedStep {
	return []namedStep{
		{"ddlDropSizeHistory", ddlDropSizeHistory},                         // forecast-06
		{"ddlDecisionOpenWithheldIndex", ddlDecisionOpenWithheldIndex()},   // policy-07
		{"ddlActionQueueSchedule", ddlActionQueueSchedule},                 // policy-04
		{"ddlRolloutRunInterrupted", ddlRolloutRunInterrupted},             // executor-04b
		{"ddlNotificationLogRefs", ddlNotificationLogRefs},                 // sre-08
		{"ddlSREIncidentFollowup", ddlSREIncidentFollowup},                 // sre-08
		{"ddlSREEvidenceRefreshedEvent", ddlSREEvidenceRefreshedEvent},     // playbooks-06
		{"ddlIncidentsLogOccurrences", ddlIncidentsLogOccurrences},         // rca-07
		{"ddlExplainEstimateSource", ddlExplainEstimateSource},             // rca-04
		{"ddlGuardPrincipalChanges", ddlGuardPrincipalChanges},             // agentgov-06
		{"ddlSourceFixDeployReportedAt", ddlSourceFixDeployReportedAt},     // mcp-06
		{"ddlEvidenceSigningKey", ddlEvidenceSigningKey},                   // audit-10
		{"ddlAuditChainMarkerOps", ddlAuditChainMarkerOps},                 // audit-03b
		{"ddlPolicyApprovalsID", ddlPolicyApprovalsID},                     // audit-11
		{"ddlUsersPasswordLifecycle", ddlUsersPasswordLifecycle},           // apiauth-03
		{"ddlSchemaMigrations", ddlSchemaMigrations},                       // config-04b: ledger
		{"ddlAuditChainOpCheckValidated", ddlAuditChainOpCheckValidated},   // audit-03b
		{"ddlManagedChangeProviders", ddlManagedChangeProviders},           // advisors-03
		{"ddlSREAutonomyEvidenceExpiring", ddlSREAutonomyEvidenceExpiring}, // earned-06
		{"ddlSREAutonomyServerMajor", ddlSREAutonomyServerMajor},           // earned-07
		{"ddlGameDaySummary", ddlGameDaySummary},                           // benches-08
	}
}

// trancheThreeMigrations are the T3 steps, in landing order.
func trancheThreeMigrations() []namedStep {
	return []namedStep{
		{"ddlBaselineSamples", ddlBaselineSamples},                       // market-02
		{"ddlBudgetShadow", ddlBudgetShadow},                             // market-08
		{"ddlLLMUsageAttribution", ddlLLMUsageAttribution},               // llm-04: after ddlLLMUsage
		{"ddlNotificationOutbox", ddlNotificationOutbox},                 // alerting-06
		{"ddlCaseTickets", ddlCaseTickets},                               // market-14
		{"ddlUserDatabaseGrants", ddlUserDatabaseGrants},                 // apiauth-08
		{"ddlDatabasesDiscovered", ddlDatabasesDiscovered},               // market-06
		{"ddlDiscoveredDatabases", ddlDiscoveredDatabases},               // market-06
		{"ddlProviderAdvice", ddlProviderAdvice},                         // market-09
		{"ddlHostUsageHourly", ddlHostUsageHourly},                       // market-04
		{"ddlManagedChangeProvidersCNPG", ddlManagedChangeProvidersCNPG}, // market-12
		{"ddlGameDayExercised", ddlGameDayExercised},                     // benches-12
	}
}

// trancheFourMigrations are the T4 steps, in landing order.
func trancheFourMigrations() []namedStep {
	return []namedStep{
		{"ddlDatabasesActionCredential", ddlDatabasesActionCredential}, // security-09b
		{"ddlUserDatabaseGrantsSource", ddlUserDatabaseGrantsSource},   // apiauth-08d
		{"ddlBudgetShadowActualCancels", ddlBudgetShadowActualCancels}, // market-08b
		{"ddlAskRuns", ddlAskRuns},                                     // mcp-07b
		{"ddlNotificationManagedBy", ddlNotificationManagedBy},         // alerting-06b
		{"ddlNotificationLogDatabase", ddlNotificationLogDatabase},     // alerting-06c
	}
}

// trancheFiveMigrations are the T5 steps, after trancheFourMigrations.
func trancheFiveMigrations() []namedStep {
	return []namedStep{
		{"ddlQueryRollup", ddlQueryRollup},                                   // evid-01a
		{"ddlQueryRollupDaily", ddlQueryRollupDaily},                         // evid-01b
		{"ddlHistoryMigrationRollupTables", ddlHistoryMigrationRollupTables}, // evid-01b
		{"ddlWorkloadPacks", ddlWorkloadPacks},                               // evid-02a
		{"ddlFactsOwnerBrief", ddlFactsOwnerBrief},                           // evid-03a
		{"ddlActionOutcomeVerifier", ddlActionOutcomeVerifier},               // lab-01c
		{"ddlVerifierBacktest", ddlVerifierBacktest},                         // lab-02
		{"ddlPlans", ddlPlans},                                               // plan-01a
		{"ddlPlanEvents", ddlPlanEvents},                                     // plan-01b
		{"ddlPlanStepRun", ddlPlanStepRun},                                   // plan-02a
		{"ddlPlanStepApproval", ddlPlanStepApproval},                         // plan-02b
		{"ddlPlanOperatorSteps", ddlPlanOperatorSteps},                       // plan-02d
		{"ddlPlanApprovalModes", ddlPlanApprovalModes},                       // plan-03a
		{"ddlWorkloadPseudonymKey", ddlWorkloadPseudonymKey},                 // evid-02c
	}
}
