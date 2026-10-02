package earned

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/policy"
)

// Action classes come from the executor's typed contracts. Each class
// declares its reversibility, and the reversibility caps the level:
// irreversible classes never exceed L1 (AI-SRE-SPEC §7.3).

func request(actionType, feature, sql string, rollback policy.RollbackClass) policy.ActionRequest {
	req := policy.ActionRequest{Feature: feature, SQL: sql}
	if actionType != "" {
		req.Contract = &policy.ActionContract{ActionType: actionType,
			RiskTier: policy.RiskSafe, RollbackClass: rollback}
	}
	return req
}

func TestClassForMapsExecutorContracts(t *testing.T) {
	cases := []struct {
		name                   string
		actionType, feature, s string
		want                   ActionClass
	}{
		{"custodian freeze", "vacuum_table", "freeze", "VACUUM (FREEZE) public.t", ClassFreeze},
		{"plain vacuum", "vacuum_table", "vacuum", "VACUUM public.t", ClassVacuum},
		{"analyze", "analyze_table", "analyze", "ANALYZE public.t", ClassAnalyze},
		{"autovacuum tuning", "set_table_autovacuum", "autovacuum_tuning", "", ClassAutovacuumTuning},
		{"index create", "create_index_concurrently", "index", "", ClassIndexCreate},
		{"index drop", "drop_unused_index", "index", "", ClassIndexDrop},
		{"revert created index", "revert_created_index", "index", "", ClassIndexDrop},
		{"reindex", "reindex_concurrently", "index", "", ClassReindex},
		{"statistics", "create_statistics", "", "", ClassStatistics},
		{"apply hint", "apply_query_hint", "query_hint", "", ClassQueryHint},
		{"retire hint", "retire_query_hint", "query_hint", "", ClassQueryHint},
		{"wal bound", "alter_system_guc", "config_guc",
			"ALTER SYSTEM SET max_slot_wal_keep_size = '1024MB'", ClassWALBound},
		{"system guc", "alter_system_guc", "config_guc",
			"ALTER SYSTEM SET work_mem = '64MB'", ClassConfigGUC},
		{"database guc", "alter_database_guc", "config_guc", "", ClassConfigGUC},
		{"role work_mem", "promote_role_work_mem", "", "", ClassConfigGUC},
		{"cancel", "cancel_backend", "backend_signal", "", ClassBackendCancel},
		{"terminate", "terminate_backend", "backend_signal", "", ClassBackendTerminate},
		{"schema change", "alter_table", "schema_change", "", ClassSchemaChange},
		{"sequence", "prepare_sequence_capacity_migration", "", "", ClassSequenceMigration},
		{"slot drop without a contract", "", "wal",
			"SELECT pg_drop_replication_slot('s1')", ClassSlotDrop},
		{"unknown action", "invent_something", "", "", ClassUnclassified},
		{"no contract, no sql", "", "", "", ClassUnclassified},
	}
	for _, c := range cases {
		got := ClassFor(request(c.actionType, c.feature, c.s, policy.RollbackReversible))
		if got != c.want {
			t.Errorf("%s: ClassFor = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestIrreversibleClassesNeverExceedL1(t *testing.T) {
	for _, spec := range Classes() {
		switch spec.Reversibility {
		case Irreversible:
			if spec.Cap > L1 {
				t.Errorf("%s is irreversible but capped at %v", spec.Class, spec.Cap)
			}
		case MitigationOnly:
			if spec.Cap > L2 {
				t.Errorf("%s is mitigation-only but capped at %v", spec.Class, spec.Cap)
			}
		case Reversible:
			if spec.Cap > L3 {
				t.Errorf("%s capped above L3: %v", spec.Class, spec.Cap)
			}
		default:
			t.Errorf("%s has unknown reversibility %q", spec.Class, spec.Reversibility)
		}
		if spec.Cap > MaxGrantable || spec.Cap < L0 {
			t.Errorf("%s cap %v outside L0..L3", spec.Class, spec.Cap)
		}
	}
	for _, class := range []ActionClass{ClassBackendTerminate, ClassSlotDrop,
		ClassSchemaChange, ClassSequenceMigration, ClassUnclassified} {
		if CapFor(class) != L1 {
			t.Errorf("CapFor(%s) = %v, want L1", class, CapFor(class))
		}
	}
	if CapFor("never_heard_of_it") != L1 {
		t.Error("an unknown class must be capped at L1")
	}
}

// Product calls recorded in the M7 report: cancel is mitigation-only (L2
// at most, the M5 approved cancel); a WAL bound can invalidate a slot,
// which cannot be undone, so it never auto-executes; global GUC changes
// stay behind a human.
func TestProductCallCaps(t *testing.T) {
	for class, want := range map[ActionClass]Level{
		ClassBackendCancel: L2, ClassWALBound: L2, ClassConfigGUC: L2,
		ClassFreeze: L3, ClassAnalyze: L3, ClassIndexCreate: L3, ClassVacuum: L3,
		ClassAutovacuumTuning: L3,
	} {
		if got := CapFor(class); got != want {
			t.Errorf("CapFor(%s) = %v, want %v", class, got, want)
		}
	}
}

func TestRollbackCapFollowsTheContract(t *testing.T) {
	for class, want := range map[policy.RollbackClass]Level{
		policy.RollbackReversible: L3, policy.RollbackNoRollbackNeeded: L3,
		policy.RollbackClass("mitigation_only"): L2,
		policy.RollbackNotReversible:            L1, policy.RollbackForwardFixOnly: L1,
		policy.RollbackApplication: L1, policy.RollbackNotApplicable: L1, "": L1,
		policy.RollbackClass("whatever"): L1,
	} {
		if got := RollbackCap(class); got != want {
			t.Errorf("RollbackCap(%q) = %v, want %v", class, got, want)
		}
	}
}

func TestFamiliesAndApplicability(t *testing.T) {
	families := Families()
	if len(families) != 11 {
		t.Fatalf("families = %v, want the 11 R1/R2 families", families)
	}
	for _, f := range families {
		if !KnownFamily(f) {
			t.Errorf("%s not known", f)
		}
		if len(ApplicableClasses(f)) == 0 {
			t.Errorf("%s has no applicable class", f)
		}
	}
	for _, f := range []Family{"", "operator", "change", "shell", "Lock_Blocking"} {
		if KnownFamily(f) {
			t.Errorf("%q must not be a known family", f)
		}
	}
	if !Applicable(FamilyWAL, ClassWALBound) || !Applicable(FamilyWraparound, ClassFreeze) ||
		!Applicable(FamilyLockBlocking, ClassBackendCancel) {
		t.Fatal("core pairs are not applicable")
	}
	if Applicable(FamilyLockBlocking, ClassIndexDrop) || Applicable("shell", ClassFreeze) {
		t.Fatal("unrelated pairs are applicable")
	}
}
