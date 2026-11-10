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
	return append(steps, namedSteps(trancheOneMigrations())...)
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
