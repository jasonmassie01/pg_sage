package executor

import (
	"slices"
	"strings"
	"testing"
)

// executor-30: contractRegistry is the one mapping from action type to
// contract; ContractActionTypes and ContractForActionType both read it.

func expectIndexPanic(t *testing.T, want string, rows []contractRow) {
	t.Helper()
	defer func() {
		r := recover()
		msg, _ := r.(string)
		if r == nil || !strings.Contains(msg, want) {
			t.Fatalf("newContractIndex panic = %v, want one containing %q", r, want)
		}
	}()
	newContractIndex(rows)
}

// Invalid input: a duplicate, empty or builder-less row fails at init.
func TestNewContractIndex_PanicsOnBadRows(t *testing.T) {
	build := func(actionType string) ActionContract { return ActionContract{} }
	expectIndexPanic(t, `duplicate contract action type "vacuum_table"`, []contractRow{
		{"vacuum_table", build}, {"analyze_table", build}, {"vacuum_table", build},
	})
	expectIndexPanic(t, "empty contract action type", []contractRow{{"", build}})
	expectIndexPanic(t, `contract "analyze_table" has no builder`,
		[]contractRow{{"analyze_table", nil}})
}

// Nil/empty: an empty registry yields an empty index, not a panic.
func TestNewContractIndex_EmptyRegistryIsEmpty(t *testing.T) {
	if got := newContractIndex(nil); len(got) != 0 {
		t.Fatalf("newContractIndex(nil) has %d entries, want 0", len(got))
	}
}

// Happy path: each row builds the contract of its own type, and the
// exported list is the registry's order (they can no longer diverge).
func TestContractRegistry_RowsBuildTheirOwnTypeInListOrder(t *testing.T) {
	types := make([]string, 0, len(contractRegistry))
	for _, row := range contractRegistry {
		if got := row.build(row.actionType).ActionType; got != row.actionType {
			t.Errorf("row %q builds a %q contract", row.actionType, got)
		}
		types = append(types, row.actionType)
	}
	if !slices.Equal(ContractActionTypes(), types) {
		t.Fatalf("ContractActionTypes = %q, want registry order %q",
			ContractActionTypes(), types)
	}
	if len(contractIndex) != len(contractRegistry) {
		t.Fatalf("index has %d entries, registry %d", len(contractIndex), len(contractRegistry))
	}
}
