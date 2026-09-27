package executor

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// Azure Database for PostgreSQL runs the portable SQL actions; ALTER SYSTEM
// is unavailable there (server parameters go through Azure).
func TestAzureSupportsPortableActions(t *testing.T) {
	for _, actionType := range []string{
		"analyze_table", "vacuum_table", "create_index_concurrently",
		"drop_unused_index", "reindex_concurrently", "set_table_autovacuum",
		"cancel_backend", "terminate_backend", "alter_database_guc", "alter_table",
	} {
		contract, ok := ContractForActionType(actionType)
		if !ok || !contractSupportsProvider(contract, "azure") {
			t.Errorf("%s does not support azure", actionType)
		}
	}
	contract, _ := ContractForActionType("alter_system_guc")
	if contractSupportsProvider(contract, "azure") {
		t.Fatal("azure must not execute ALTER SYSTEM")
	}
}

func TestAzureAnalyzeAuthorizedByGate(t *testing.T) {
	cfg := &config.Config{CloudEnvironment: "azure",
		Trust: config.TrustConfig{Level: "autonomous", Tier3Safe: true}}
	got := policyVerdict(AnalyzeTableContract(), verdictInput{
		cfg: cfg, rampStart: time.Now().Add(-10 * 24 * time.Hour),
	})
	if got.Decision != PolicyDecisionExecute || got.Provider != "azure" {
		t.Fatalf("azure analyze = %#v, want execute on azure", got)
	}
}

func TestAzureManagedConfigUsesServerParameters(t *testing.T) {
	if got := managedConfigMechanism("azure"); got != ManagedServerParameter {
		t.Fatalf("mechanism = %q, want server_parameter", got)
	}
	guidance := managedConfigGuidance("azure", "work_mem")
	for _, want := range []string{"az postgres flexible-server parameter set", "work_mem"} {
		if !strings.Contains(guidance, want) {
			t.Fatalf("guidance %q lacks %q", guidance, want)
		}
	}
}
