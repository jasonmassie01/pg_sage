package earned

import (
	"strings"

	"github.com/pg-sage/sidecar/internal/policy"
)

// Family is a Sage SRE incident family (the investigator's trigger kinds
// and the M6 families).
type Family string

// Incident families.
const (
	FamilyLockBlocking   Family = "lock_blocking"
	FamilyConnections    Family = "connection_pressure"
	FamilyWAL            Family = "wal_retention"
	FamilyPlanRegression Family = "plan_regression"
	FamilyCheckpoint     Family = "checkpoint_storm"
	FamilyTempFiles      Family = "temp_file_explosion"
	FamilyReplicationLag Family = "replication_lag"
	FamilyLWLock         Family = "lwlock_contention"
	FamilyWraparound     Family = "wraparound_runway"
	FamilyDiskWAL        Family = "disk_wal_runway"
	FamilySequence       Family = "sequence_runway"
)

// ActionClass groups executor action types the ledger levels together.
type ActionClass string

// Action classes. AllClasses addresses every class of a family in a
// downgrade.
const (
	ClassFreeze            ActionClass = "freeze"
	ClassVacuum            ActionClass = "vacuum"
	ClassAnalyze           ActionClass = "analyze"
	ClassAutovacuumTuning  ActionClass = "autovacuum_tuning"
	ClassIndexCreate       ActionClass = "index_create"
	ClassIndexDrop         ActionClass = "index_drop"
	ClassReindex           ActionClass = "reindex"
	ClassStatistics        ActionClass = "statistics"
	ClassQueryHint         ActionClass = "query_hint"
	ClassConfigGUC         ActionClass = "config_guc"
	ClassWALBound          ActionClass = "wal_bound"
	ClassSlotDrop          ActionClass = "slot_drop"
	ClassBackendCancel     ActionClass = "backend_cancel"
	ClassBackendTerminate  ActionClass = "backend_terminate"
	ClassSchemaChange      ActionClass = "schema_change"
	ClassSequenceMigration ActionClass = "sequence_migration"
	ClassRetention         ActionClass = "retention"
	ClassIndexReplace      ActionClass = "index_replace"
	ClassUnclassified      ActionClass = "unclassified"
	AllClasses             ActionClass = "*"
)

// Reversibility is a class's repair-contract reversibility (§7.2).
type Reversibility string

// Reversibility classes.
const (
	Reversible     Reversibility = "reversible"
	MitigationOnly Reversibility = "mitigation_only"
	Irreversible   Reversibility = "irreversible"
)

// ClassSpec declares one action class.
type ClassSpec struct {
	Class         ActionClass   `json:"class"`
	Reversibility Reversibility `json:"reversibility"`
	Cap           Level         `json:"cap"`
	ActionTypes   []string      `json:"action_types"`
	Description   string        `json:"description"`
}

// classSpecs: caps follow reversibility (irreversible never above L1,
// mitigation-only never above L2). Product calls: a WAL bound can
// invalidate a slot, which cannot be undone, so it is mitigation-only;
// GUC changes stay behind a human (global GUC changes were excluded
// through R2).
var classSpecs = []ClassSpec{
	{ClassFreeze, Reversible, L3, []string{"vacuum_table"}, "VACUUM (FREEZE) of one table"},
	{ClassVacuum, Reversible, L3, []string{"vacuum_table"}, "non-FULL VACUUM of one table"},
	{ClassAnalyze, Reversible, L3, []string{"analyze_table"}, "ANALYZE of one table"},
	{ClassAutovacuumTuning, Reversible, L3, []string{"set_table_autovacuum"},
		"per-table autovacuum storage parameters"},
	{ClassIndexCreate, Reversible, L3, []string{"create_index_concurrently"},
		"CREATE INDEX CONCURRENTLY"},
	{ClassIndexDrop, Reversible, L3, []string{"drop_unused_index", "revert_created_index"},
		"DROP INDEX CONCURRENTLY with a recorded definition"},
	{ClassReindex, Reversible, L3, []string{"reindex_concurrently"}, "REINDEX CONCURRENTLY"},
	{ClassStatistics, Reversible, L3, []string{"create_statistics", "revert_created_statistics"},
		"CREATE STATISTICS (pg_sage form) with its ANALYZE"},
	{ClassQueryHint, Reversible, L3, []string{"apply_query_hint", "retire_query_hint"},
		"pg_hint_plan hint rows"},
	{ClassConfigGUC, Reversible, L2, []string{"alter_system_guc", "alter_database_guc",
		"promote_role_work_mem"}, "configuration parameters"},
	{ClassWALBound, MitigationOnly, L2, []string{"alter_system_guc"},
		"max_slot_wal_keep_size bound (a slot past it is lost)"},
	{ClassSlotDrop, Irreversible, L1, nil, "pg_drop_replication_slot"},
	{ClassBackendCancel, MitigationOnly, L2, []string{"cancel_backend"},
		"pg_cancel_backend of one evidence-matched backend"},
	{ClassBackendTerminate, Irreversible, L1, []string{"terminate_backend"},
		"pg_terminate_backend"},
	{ClassSchemaChange, Irreversible, L1, []string{"alter_table"}, "ALTER TABLE"},
	{ClassSequenceMigration, Irreversible, L1, []string{"prepare_sequence_capacity_migration"},
		"sequence capacity migration"},
	{ClassRetention, Irreversible, L1, []string{"retention_delete"},
		"bounded retention delete of user rows"},
	// Product call (roadmap 2.3): reversible (the old index is re-created
	// from its kept definition), but two non-atomic steps with partial
	// states stay a one-click handoff.
	{ClassIndexReplace, Reversible, L2, []string{"replace_index"},
		"CREATE INDEX CONCURRENTLY a wider index, then DROP INDEX CONCURRENTLY the " +
			"index it subsumes (soft drop)"},
	{ClassUnclassified, Irreversible, L1, nil, "any action without a known class"},
}

// Classes lists every action class.
func Classes() []ClassSpec { return append([]ClassSpec(nil), classSpecs...) }

// Spec returns a class's declaration.
func Spec(class ActionClass) (ClassSpec, bool) {
	for _, s := range classSpecs {
		if s.Class == class {
			return s, true
		}
	}
	return ClassSpec{}, false
}

// CapFor is the highest level a class can reach; unknown classes L1.
func CapFor(class ActionClass) Level {
	if s, ok := Spec(class); ok {
		return s.Cap
	}
	return L1
}

// RollbackCap is the highest level a contract's rollback class allows.
func RollbackCap(class policy.RollbackClass) Level {
	switch class {
	case policy.RollbackReversible, policy.RollbackNoRollbackNeeded:
		return L3
	case rollbackMitigationOnly:
		return L2
	}
	return L1
}

// rollbackMitigationOnly is M5's mitigation-only rollback class.
const rollbackMitigationOnly policy.RollbackClass = "mitigation_only"

// ClassFor derives the action class of a gate request from its typed
// contract (and, for the shared action types, its feature or SQL).
func ClassFor(req policy.ActionRequest) ActionClass {
	upper := strings.ToUpper(req.SQL)
	if req.Contract == nil || req.Contract.ActionType == "" {
		if strings.Contains(upper, "PG_DROP_REPLICATION_SLOT") {
			return ClassSlotDrop
		}
		return ClassUnclassified
	}
	switch req.Contract.ActionType {
	case "vacuum_table":
		if req.Feature == string(policy.ChangeFreeze) {
			return ClassFreeze
		}
		return ClassVacuum
	case "alter_system_guc":
		if strings.Contains(upper, "MAX_SLOT_WAL_KEEP_SIZE") {
			return ClassWALBound
		}
		return ClassConfigGUC
	}
	for _, s := range classSpecs {
		for _, actionType := range s.ActionTypes {
			if actionType == req.Contract.ActionType {
				return s.Class
			}
		}
	}
	return ClassUnclassified
}

// applicable: which classes can remediate which family. A pair outside
// this table never rises above L1. Product call recorded in the M7 report.
var applicable = map[Family][]ActionClass{
	FamilyLockBlocking: {ClassBackendCancel, ClassBackendTerminate},
	FamilyConnections:  {ClassBackendCancel, ClassBackendTerminate, ClassConfigGUC},
	FamilyWAL:          {ClassWALBound, ClassSlotDrop},
	FamilyPlanRegression: {ClassIndexCreate, ClassAnalyze, ClassStatistics,
		ClassQueryHint},
	FamilyCheckpoint:     {ClassConfigGUC},
	FamilyTempFiles:      {ClassConfigGUC, ClassBackendCancel},
	FamilyReplicationLag: {ClassBackendCancel, ClassConfigGUC},
	FamilyLWLock:         {ClassConfigGUC},
	FamilyWraparound: {ClassFreeze, ClassVacuum, ClassAutovacuumTuning,
		ClassBackendCancel, ClassBackendTerminate},
	FamilyDiskWAL:  {ClassWALBound, ClassSlotDrop},
	FamilySequence: {ClassSequenceMigration},
	// Self-initiated trust families (roadmap 1.2, selfinit.go).
	FamilyTuning: {ClassIndexCreate, ClassConfigGUC, ClassAutovacuumTuning,
		ClassQueryHint, ClassStatistics, ClassIndexReplace},
	FamilyHygiene: {ClassIndexDrop, ClassVacuum, ClassAnalyze, ClassRetention,
		ClassReindex},
}

var familyOrder = []Family{FamilyLockBlocking, FamilyConnections, FamilyWAL,
	FamilyPlanRegression, FamilyCheckpoint, FamilyTempFiles, FamilyReplicationLag,
	FamilyLWLock, FamilyWraparound, FamilyDiskWAL, FamilySequence}

// Families lists the incident families (AllFamilies adds the
// self-initiated trust families).
func Families() []Family { return append([]Family(nil), familyOrder...) }

// KnownFamily reports a shipped incident family.
func KnownFamily(f Family) bool {
	_, ok := applicable[f]
	return ok
}

// ApplicableClasses lists the classes that remediate f.
func ApplicableClasses(f Family) []ActionClass {
	return append([]ActionClass(nil), applicable[f]...)
}

// Applicable reports whether class remediates family.
func Applicable(f Family, class ActionClass) bool {
	for _, c := range applicable[f] {
		if c == class {
			return true
		}
	}
	return false
}

// knownClass reports a declared class.
func knownClass(class ActionClass) bool {
	_, ok := Spec(class)
	return ok
}

// ClassForActionType is the class an executor action type is judged under
// when it remediates an incident family; empty for read-only diagnostics
// and unknown types (vacuum_table outside a freeze is ClassVacuum).
func ClassForActionType(actionType string) ActionClass {
	for _, s := range classSpecs {
		for _, t := range s.ActionTypes {
			if t == actionType {
				return ClassFor(policy.ActionRequest{
					Contract: &policy.ActionContract{ActionType: actionType}})
			}
		}
	}
	return ""
}
