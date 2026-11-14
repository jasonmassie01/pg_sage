package executor

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"reflect"
	"slices"
	"sync"
	"testing"
)

// executor-30: the contract decides risk tier, rollback class, provider
// support and verification for every action, so every field of every
// contract is pinned in testdata/contracts.golden.json. A new or changed
// contract regenerates the file in the same change, so the diff shows it:
//
//	go test ./internal/executor -run TestContractGolden -update-contracts
//
// State transitions and integration do not apply: contracts are pure values
// built without a database; the gate's DB tests in internal/policy cover use.

var updateContractGolden = flag.Bool("update-contracts", false,
	"rewrite testdata/contracts.golden.json from ContractForActionType")

const contractGoldenPath = "testdata/contracts.golden.json"

// pinnedContractActionTypes is ContractActionTypes as of master 61152ef3,
// order included: fleet readiness iterates it, so its order is output order.
var pinnedContractActionTypes = []string{
	"analyze_table", "alter_system_guc", "alter_database_guc",
	"diagnose_lock_blockers", "diagnose_runaway_query", "diagnose_connection_exhaustion",
	"diagnose_wal_replication", "diagnose_standby_conflicts",
	"prepare_sequence_capacity_migration", "cancel_backend", "terminate_backend",
	"vacuum_table", "diagnose_freeze_blockers", "set_table_autovacuum",
	"diagnose_vacuum_pressure", "plan_bloat_remediation", "reindex_concurrently",
	"prepare_query_rewrite", "promote_role_work_mem", "set_role_timeout",
	"set_role_connection_limit", "retire_query_hint",
	"apply_query_hint", "investigate_query_plan", "create_statistics",
	"prepare_parameterized_query", "retention_delete", "revert_created_index",
	"restore_dropped_index", "replace_index", "revert_created_statistics",
	"guard_role_ensure", "guard_role_retire", "guard_freeze",
	"guard_kill", "guard_unfreeze", "guard_grant",
	"guard_revoke", "create_index_concurrently", "drop_unused_index",
	"alter_table", "ddl_preflight",
}

// currentContracts builds every contract in ContractActionTypes order.
func currentContracts(t *testing.T) []ActionContract {
	t.Helper()
	types := ContractActionTypes()
	out := make([]ActionContract, 0, len(types))
	for _, actionType := range types {
		c, ok := ContractForActionType(actionType)
		if !ok {
			t.Fatalf("ContractForActionType(%q) = false for a listed type", actionType)
		}
		out = append(out, c)
	}
	return out
}

func marshalContracts(t *testing.T, contracts []ActionContract) []byte {
	t.Helper()
	data, err := json.MarshalIndent(contracts, "", "  ")
	if err != nil {
		t.Fatalf("marshal contracts: %v", err)
	}
	return append(data, '\n')
}

// readContractGolden strips carriage returns: checkouts may convert LF.
func readContractGolden(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(contractGoldenPath)
	if err != nil {
		t.Fatalf("read %s (regenerate with -update-contracts): %v",
			contractGoldenPath, err)
	}
	return bytes.ReplaceAll(data, []byte("\r"), nil)
}

// Happy path: every contract equals its golden copy, field for field.
func TestContractGolden_EveryContractMatchesGolden(t *testing.T) {
	got := marshalContracts(t, currentContracts(t))
	if *updateContractGolden {
		if err := os.WriteFile(contractGoldenPath, got, 0o644); err != nil {
			t.Fatalf("write %s: %v", contractGoldenPath, err)
		}
	}
	want := readContractGolden(t)
	if bytes.Equal(got, want) {
		return
	}
	var wantContracts []ActionContract
	if err := json.Unmarshal(want, &wantContracts); err != nil {
		t.Fatalf("golden is not a contract list: %v", err)
	}
	for _, c := range currentContracts(t) {
		i := slices.IndexFunc(wantContracts, func(w ActionContract) bool {
			return w.ActionType == c.ActionType
		})
		if i < 0 {
			t.Errorf("%s: missing from golden", c.ActionType)
		} else if !reflect.DeepEqual(c, wantContracts[i]) {
			t.Errorf("%s: contract differs from golden:\n got %+v\nwant %+v",
				c.ActionType, c, wantContracts[i])
		}
	}
	t.Fatalf("contracts differ from %s (order or content)", contractGoldenPath)
}

// Boundary: the golden file has one valid contract per listed type, in
// order, and each golden contract is keyed by its own action type.
func TestContractGolden_CountOrderAndValidity(t *testing.T) {
	var golden []ActionContract
	if err := json.Unmarshal(readContractGolden(t), &golden); err != nil {
		t.Fatalf("parse golden: %v", err)
	}
	types := ContractActionTypes()
	if len(golden) != len(types) || len(golden) < 42 {
		t.Fatalf("golden has %d contracts, ContractActionTypes has %d, want equal and >= 42",
			len(golden), len(types))
	}
	for i, c := range golden {
		if c.ActionType != types[i] {
			t.Errorf("golden[%d].ActionType = %q, want %q", i, c.ActionType, types[i])
		}
		if err := c.Validate(); err != nil {
			t.Errorf("golden %s fails Validate: %v", c.ActionType, err)
		}
	}
}

// Boundary: ContractActionTypes keeps today's set and order exactly.
func TestContractActionTypes_EqualsPinnedOrderedList(t *testing.T) {
	got := ContractActionTypes()
	if !slices.Equal(got, pinnedContractActionTypes) {
		t.Fatalf("ContractActionTypes changed:\n got %q\nwant %q",
			got, pinnedContractActionTypes)
	}
}

// Invalid input: lookups are exact; near misses get no contract at all.
func TestContractForActionType_RejectsNearMissesWithZeroContract(t *testing.T) {
	for _, actionType := range []string{
		"", "ANALYZE_TABLE", "analyze_table ", " analyze_table", "Vacuum_Table",
		"no_such_action", "analyze_table\x00", "drop_unused_index;",
	} {
		c, ok := ContractForActionType(actionType)
		if ok {
			t.Errorf("ContractForActionType(%q) = true, want false", actionType)
		}
		if !reflect.DeepEqual(c, ActionContract{}) {
			t.Errorf("ContractForActionType(%q) = %+v, want zero contract", actionType, c)
		}
	}
}

// Nil/empty: the zero contract fails Validate with today's first error, and
// each later rule still fires on its own (Validate must not change).
func TestActionContract_ValidateRulesUnchanged(t *testing.T) {
	full, _ := ContractForActionType("vacuum_table")
	noPost, noRollback := full, full
	noPost.PostChecks = nil
	noRollback.RollbackClass = ""
	noTier := full
	noTier.BaseRiskTier = ""
	for _, tc := range []struct {
		name string
		c    ActionContract
		want string
	}{
		{"zero", ActionContract{}, "action contract missing type or risk tier"},
		{"no tier", noTier, "action contract missing type or risk tier"},
		{"no post-checks", noPost, "action contract missing post-checks"},
		{"no rollback", noRollback, "action contract missing rollback class"},
	} {
		err := tc.c.Validate()
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s: Validate() = %v, want %q", tc.name, err, tc.want)
		}
	}
	if err := full.Validate(); err != nil {
		t.Fatalf("vacuum_table Validate() = %v, want nil", err)
	}
}

// mutateContractSlices overwrites and appends to every slice field of c, so
// a contract sharing a backing array with another call would show it.
func mutateContractSlices(c *ActionContract) {
	v := reflect.ValueOf(c).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if f.Kind() != reflect.Slice {
			continue
		}
		for j := 0; j < f.Len(); j++ {
			f.Index(j).SetString("mutated")
		}
		f.Set(reflect.Append(f, reflect.ValueOf("appended")))
	}
}

// CHECK-03: mutating one result never reaches the next call.
func TestContractForActionType_MutatedResultLeavesNextCallUnchanged(t *testing.T) {
	first, _ := ContractForActionType("vacuum_table")
	want, _ := ContractForActionType("vacuum_table")
	first.ProviderSupport[0] = "mutated"
	if next, _ := ContractForActionType("vacuum_table"); !reflect.DeepEqual(next, want) ||
		next.ProviderSupport[0] == "mutated" {
		t.Fatalf("vacuum_table ProviderSupport shared across calls: %q", next.ProviderSupport)
	}
	for _, actionType := range ContractActionTypes() {
		want, _ := ContractForActionType(actionType)
		got, _ := ContractForActionType(actionType)
		mutateContractSlices(&got)
		if next, _ := ContractForActionType(actionType); !reflect.DeepEqual(next, want) {
			t.Errorf("%s: mutating one result changed the next:\n got %+v\nwant %+v",
				actionType, next, want)
		}
	}
}

// Concurrent access: 16 goroutines build and mutate every contract at once;
// each must still see the golden value (CI runs this under -race).
func TestContractForActionType_ConcurrentCallsAreIndependent(t *testing.T) {
	want := map[string]ActionContract{}
	for _, c := range currentContracts(t) {
		want[c.ActionType] = c
	}
	var wg sync.WaitGroup
	errs := make(chan string, 16*len(want))
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, actionType := range ContractActionTypes() {
				got, ok := ContractForActionType(actionType)
				if !ok || !reflect.DeepEqual(got, want[actionType]) {
					errs <- actionType
				}
				mutateContractSlices(&got)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for actionType := range errs {
		t.Errorf("%s: concurrent call saw a changed contract", actionType)
	}
}
