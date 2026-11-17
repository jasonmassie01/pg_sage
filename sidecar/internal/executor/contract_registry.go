package executor

import "fmt"

// contractRow maps one action type to the builder of its contract. The
// builder receives the action type, as the shared constructors need it.
type contractRow struct {
	actionType string
	build      func(actionType string) ActionContract
}

// fixed adapts a constructor whose contract names its own action type.
func fixed(build func() ActionContract) func(string) ActionContract {
	return func(string) ActionContract { return build() }
}

// contractRegistry is every action type with a contract, in the order
// ContractActionTypes returns them (fleet readiness output follows it). A new
// action type is one row here plus its constructor, and a regenerated
// testdata/contracts.golden.json in the same change.
var contractRegistry = []contractRow{
	{"analyze_table", fixed(AnalyzeTableContract)},
	{"alter_system_guc", alterSystemGUCContract},
	{"alter_database_guc", alterDatabaseGUCContract},
	{"diagnose_lock_blockers", fixed(incidentDiagnoseLockBlockersContract)},
	{"diagnose_runaway_query", fixed(incidentDiagnoseRunawayQueryContract)},
	{"diagnose_connection_exhaustion", fixed(incidentDiagnoseConnectionExhaustionContract)},
	{"diagnose_wal_replication", fixed(incidentDiagnoseWALReplicationContract)},
	{"diagnose_standby_conflicts", fixed(incidentDiagnoseStandbyConflictsContract)},
	{"prepare_sequence_capacity_migration", fixed(incidentSequenceCapacityMigrationContract)},
	{"cancel_backend", fixed(incidentCancelBackendContract)},
	{"terminate_backend", fixed(incidentTerminateBackendContract)},
	{"vacuum_table", fixed(vacuumTableContract)},
	{"diagnose_freeze_blockers", fixed(diagnoseFreezeBlockersContract)},
	{"set_table_autovacuum", fixed(setTableAutovacuumContract)},
	{"diagnose_vacuum_pressure", fixed(diagnoseVacuumPressureContract)},
	{"plan_bloat_remediation", fixed(planBloatRemediationContract)},
	{"reindex_concurrently", fixed(reindexConcurrentlyContract)},
	{"prepare_query_rewrite", fixed(prepareQueryRewriteContract)},
	{"promote_role_work_mem", fixed(promoteRoleWorkMemContract)},
	{ActionTypeSetRoleTimeout, fixed(setRoleTimeoutContract)},
	{ActionTypeSetRoleConnectionLimit, fixed(setRoleConnectionLimitContract)},
	{"retire_query_hint", fixed(retireQueryHintContract)},
	{"apply_query_hint", fixed(applyQueryHintContract)},
	{"investigate_query_plan", fixed(investigateQueryPlanContract)},
	{"create_statistics", fixed(createStatisticsContract)},
	{ActionTypeSetColumnStatistics, fixed(setColumnStatisticsContract)},
	{"prepare_parameterized_query", fixed(prepareParameterizedQueryContract)},
	{"retention_delete", fixed(retentionDeleteContract)},
	{"revert_created_index", fixed(revertCreatedIndexContract)},
	{ActionTypeRestoreDroppedIndex, fixed(restoreDroppedIndexContract)},
	{ActionTypeReplaceIndex, fixed(replaceIndexContract)},
	{"revert_created_statistics", fixed(revertCreatedStatisticsContract)},
	{ActionTypeGuardRoleEnsure, fixed(guardRoleEnsureContract)},
	{ActionTypeGuardRoleRetire, fixed(guardRoleRetireContract)},
	{ActionTypeGuardFreeze, guardFreezeContract},
	{ActionTypeGuardKill, guardKillContract},
	{ActionTypeGuardUnfreeze, fixed(guardUnfreezeContract)},
	{ActionTypeGuardGrant, fixed(guardGrantContract)},
	{ActionTypeGuardRevoke, fixed(guardRevokeContract)},
	{ActionTypeRevokePublicGrant, fixed(revokePublicGrantContract)},
	{ActionTypeEnableRLS, fixed(enableRLSContract)},
	{"create_index_concurrently", createIndexConcurrentlyContract},
	{"drop_unused_index", dropUnusedIndexContract},
	{"alter_table", alterTableContract},
	{"ddl_preflight", ddlPreflightContract},
}

// contractIndex is read-only after package initialisation.
var contractIndex = newContractIndex(contractRegistry)

// newContractIndex fails closed at start-up: a duplicate type would make one
// contract silently shadow another, so it panics instead.
func newContractIndex(rows []contractRow) map[string]func(string) ActionContract {
	index := make(map[string]func(string) ActionContract, len(rows))
	for _, row := range rows {
		switch {
		case row.actionType == "":
			panic("executor: empty contract action type in contractRegistry")
		case row.build == nil:
			panic(fmt.Sprintf("executor: contract %q has no builder", row.actionType))
		case index[row.actionType] != nil:
			panic(fmt.Sprintf("executor: duplicate contract action type %q", row.actionType))
		}
		index[row.actionType] = row.build
	}
	return index
}

func guardFreezeContract(actionType string) ActionContract {
	return guardContainContract(actionType, "one principal, cluster-wide")
}

func guardKillContract(actionType string) ActionContract {
	return guardContainContract(actionType, "every agent role of the scope")
}

// ContractForActionType returns a freshly built contract for actionType, or
// a zero contract and false when the type has none (lookups are exact).
func ContractForActionType(actionType string) (ActionContract, bool) {
	build, ok := contractIndex[actionType]
	if !ok {
		return ActionContract{}, false
	}
	return build(actionType), true
}

// ContractActionTypes returns every action type that has a contract, in
// registry order. Readiness derives each provider's supported families
// from these.
func ContractActionTypes() []string {
	types := make([]string, len(contractRegistry))
	for i, row := range contractRegistry {
		types[i] = row.actionType
	}
	return types
}
