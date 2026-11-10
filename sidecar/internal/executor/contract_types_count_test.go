package executor

import (
	"os"
	"regexp"
	"testing"
)

// Boundary: every case of the ContractForActionType switch, including
// cases named by a constant, appears in ContractActionTypes, so a new
// constant-named contract cannot be left out of readiness unnoticed.
func TestContractActionTypesCountMatchesSwitchCases(t *testing.T) {
	source, err := os.ReadFile("action_contract.go")
	if err != nil {
		t.Fatalf("read action_contract.go: %v", err)
	}
	body := regexp.MustCompile(`(?s)func ContractForActionType.*?\r?\n}\r?\n`).Find(source)
	if body == nil {
		t.Fatal("ContractForActionType not found in action_contract.go")
	}
	cases := regexp.MustCompile(`(?m)^\tcase ([^:]+):`).FindAllSubmatch(body, -1)
	commas := regexp.MustCompile(`,`)
	labels := 0
	for _, c := range cases {
		labels += 1 + len(commas.FindAll(c[1], -1))
	}
	if labels == 0 {
		t.Fatal("no case labels parsed from ContractForActionType")
	}
	if got := len(ContractActionTypes()); got != labels {
		t.Fatalf("ContractActionTypes has %d entries, switch has %d case labels", got, labels)
	}
}
