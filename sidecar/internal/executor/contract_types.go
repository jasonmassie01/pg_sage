package executor

// contractActionTypeList is every action type ContractForActionType knows,
// in its order (TestContractActionTypesMatchesContracts keeps them equal).
var contractActionTypeList = []string{
	"analyze_table", "alter_system_guc", "alter_database_guc",
	"diagnose_lock_blockers", "diagnose_runaway_query", "diagnose_connection_exhaustion",
	"diagnose_wal_replication", "diagnose_standby_conflicts",
	"prepare_sequence_capacity_migration", "cancel_backend", "terminate_backend",
	"vacuum_table", "diagnose_freeze_blockers", "set_table_autovacuum",
	"diagnose_vacuum_pressure", "plan_bloat_remediation", "reindex_concurrently",
	"prepare_query_rewrite", "promote_role_work_mem", ActionTypeSetRoleTimeout,
	ActionTypeSetRoleConnectionLimit, "retire_query_hint",
	"apply_query_hint", "investigate_query_plan", "create_statistics",
	"prepare_parameterized_query", "retention_delete", "revert_created_index",
	ActionTypeRestoreDroppedIndex, ActionTypeReplaceIndex, "revert_created_statistics",
	ActionTypeGuardRoleEnsure, ActionTypeGuardRoleRetire, ActionTypeGuardFreeze,
	ActionTypeGuardKill, ActionTypeGuardUnfreeze, ActionTypeGuardGrant,
	ActionTypeGuardRevoke, "create_index_concurrently", "drop_unused_index",
	"alter_table", "ddl_preflight",
}

// ContractActionTypes returns every action type that has a contract.
// Readiness derives each provider's supported families from these.
func ContractActionTypes() []string {
	return append([]string(nil), contractActionTypeList...)
}
