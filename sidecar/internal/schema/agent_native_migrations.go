package schema

// numberedMigration gives each Phase 0 schema change a stable, ordered
// identity. The SQL remains idempotent because Bootstrap can be retried after
// a partial startup and older installations have no migration ledger.
type numberedMigration struct {
	Version int
	Name    string
	SQL     string
}

func agentNativeMigrations() []numberedMigration {
	return []numberedMigration{
		{
			Version: 2026072201,
			Name:    "standing_policy",
			SQL:     ddlAgentNativePolicy,
		},
		{
			Version: 2026072202,
			Name:    "decision_lease_baseline",
			SQL:     ddlAgentNativeLedger,
		},
		{
			Version: 2026072203,
			Name:    "durable_verification",
			SQL:     ddlAgentNativeVerification,
		},
		{
			Version: 2026072204,
			Name:    "honest_value",
			SQL:     ddlAgentNativeValue,
		},
		{
			Version: 2026072205,
			Name:    "agent_native_feature_state",
			SQL:     ddlAgentNativeFeatureState,
		},
	}
}

func agentNativeMigrationStatements() []string {
	migrations := agentNativeMigrations()
	statements := make([]string, 0, len(migrations))
	for _, migration := range migrations {
		statements = append(statements, migration.SQL)
	}
	return statements
}
