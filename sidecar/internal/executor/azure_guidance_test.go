package executor

import (
	"strings"
	"testing"
)

// advisors-03 / CHECK-07: the guidance for an Azure ALTER SYSTEM refusal
// no longer claims the azure: section makes pg_sage apply the parameter;
// only the WAL-slot cap goes through ARM, everything else is a proposal.
//
// No concurrent access tests: managedConfigGuidance is a pure function.
func TestAzureGuidanceDoesNotPromiseAnARMWrite(t *testing.T) {
	for _, provider := range []string{"azure", "azure-flexible"} {
		g := managedConfigGuidance(provider, "max_wal_size")
		// Split so CHECK-07 (grep -rn of the phrase) only finds code that says it.
		if strings.Contains(g, "so pg_sage "+"applies it") {
			t.Fatalf("%s: stale promise in %q", provider, g)
		}
		for _, want := range []string{"az postgres flexible-server parameter set",
			"--name max_wal_size", "--resource-group", "--server-name", "proposal",
			"ALTER SYSTEM is unavailable"} {
			if !strings.Contains(g, want) {
				t.Fatalf("%s: guidance %q lacks %q", provider, g, want)
			}
		}
	}
}
