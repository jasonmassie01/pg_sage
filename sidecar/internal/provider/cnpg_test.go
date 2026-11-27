package provider

import (
	"strings"
	"testing"
)

// market-12: a CloudNativePG Cluster is real PostgreSQL whose
// configuration the operator owns (spec.postgresql.parameters); ALTER
// SYSTEM is disabled, so it is managed for configuration.

// No concurrent access tests: TestLookupConcurrent covers the registry.

func TestCNPGProfile(t *testing.T) {
	for _, name := range []string{"cnpg", " CNPG ", "cloudnative-pg", "cloudnativepg"} {
		p, ok := Lookup(name)
		if !ok || p.Name != CNPG || !p.Managed || p.ConfigMechanism != ConfigClusterSpec {
			t.Errorf("Lookup(%q) = %+v, %v", name, p, ok)
		}
	}
	steps := strings.Join(PreloadSteps("cnpg", "pg_stat_statements"), " ")
	for _, want := range []string{"spec.postgresql.shared_preload_libraries",
		"pg_stat_statements", "rolling restart"} {
		if !strings.Contains(steps, want) {
			t.Errorf("preload steps %q lack %q", steps, want)
		}
	}
	if IsManaged("cnpgx") || Canonical("cnpg ") != CNPG {
		t.Fatal("only the CNPG names resolve")
	}
}
