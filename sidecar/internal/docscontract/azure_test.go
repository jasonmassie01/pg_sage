package docscontract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// advisors-03: docs/azure.md promised that configuration changes and
// restart-bound parameters are applied through ARM. Only the WAL-slot
// custodian's max_slot_wal_keep_size is; every other server parameter is a
// proposal with the az command.
//
// No concurrent-access or state-transition tests: a static check of one
// documentation page.

func readAzureDoc(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "azure.md"))
	if err != nil {
		t.Fatalf("read docs/azure.md: %v", err)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

func TestAzureDocDoesNotPromiseARMWritesForAdvisorChanges(t *testing.T) {
	doc := readAzureDoc(t)
	for _, stale := range []string{
		"Restart-bound parameters | Applied through ARM",
		"Configuration changes use Azure **server parameters** through Azure Resource Manager",
		"azure server parameters apply through ARM for server",
		"Letting pg_sage apply server parameters",
	} {
		if strings.Contains(doc, stale) {
			t.Errorf("docs/azure.md still says %q", stale)
		}
	}
}

func TestAzureDocStatesTheARMScope(t *testing.T) {
	doc := readAzureDoc(t)
	for _, want := range []string{"max_slot_wal_keep_size", "az postgres flexible-server " +
		"parameter set", "configurations/read", "server_parameter"} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/azure.md does not mention %q", want)
		}
	}
}
