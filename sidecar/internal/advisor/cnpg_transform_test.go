package advisor

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/managedparam"
)

// market-12: on a CloudNativePG Cluster ALTER SYSTEM is disabled and the
// operator owns postgresql.conf. sighup and postmaster settings become a
// cnpg managed intent (a Cluster spec patch an operator applies); user and
// superuser settings keep the ALTER DATABASE rewrite.
//
// No concurrent access tests: the gate is pure (TestTransformForCloudConcurrent
// covers it under parallel use).

// CHECK-03: with provider cnpg, a checkpoint_timeout finding has an empty
// recommended_sql and managed_change.provider == "cnpg".
func TestCNPGCheckpointTimeoutBecomesAClusterSpecIntent(t *testing.T) {
	f := gateMatrix(t, matrixFinding("ALTER SYSTEM SET checkpoint_timeout = '15min'"),
		"cnpg", "app", sighupRows())
	if f.RecommendedSQL != "" || f.RollbackSQL != "" {
		t.Fatalf("SQL survived: %q / %q", f.RecommendedSQL, f.RollbackSQL)
	}
	raw, ok := f.Detail[managedparam.DetailKey].(map[string]any)
	if !ok || raw["provider"] != "cnpg" || raw["parameter"] != "checkpoint_timeout" ||
		raw["value"] != "15min" {
		t.Fatalf("managed_change = %v", f.Detail[managedparam.DetailKey])
	}
	approval, _ := f.Detail[analyzer.DetailApprovalRequired].(string)
	if !strings.Contains(approval, "Cluster spec") || !strings.Contains(approval, "operator") {
		t.Fatalf("approval = %q", approval)
	}
	if !strings.Contains(f.Recommendation, "spec.postgresql.parameters") {
		t.Fatalf("recommendation %q does not name the Cluster spec", f.Recommendation)
	}
}

func TestCNPGUserSettingKeepsAlterDatabase(t *testing.T) {
	rows := []collector.PGSetting{{Name: "work_mem", Context: "user"}}
	f := gateMatrix(t, matrixFinding("ALTER SYSTEM SET work_mem = '64MB'"), "cnpg", "app",
		rows)
	if !strings.HasPrefix(f.RecommendedSQL, `ALTER DATABASE "app" SET work_mem`) {
		t.Fatalf("RecommendedSQL = %q, want the ALTER DATABASE rewrite", f.RecommendedSQL)
	}
	if _, ok := f.Detail[managedparam.DetailKey]; ok {
		t.Fatalf("a database-scoped change carries no managed intent: %v", f.Detail)
	}
}

func TestCNPGRestartSettingIsAnIntent(t *testing.T) {
	f := gateMatrix(t, matrixFinding("ALTER SYSTEM SET shared_buffers = '4GB'"), "cnpg",
		"app", nil)
	raw, ok := f.Detail[managedparam.DetailKey].(map[string]any)
	if f.RecommendedSQL != "" || !ok || raw["provider"] != "cnpg" {
		t.Fatalf("shared_buffers on cnpg = %+v", f)
	}
	if !IsManagedService("cnpg") || mechanismLabel("cnpg") != "Cluster spec parameter" {
		t.Fatalf("managed %v label %q", IsManagedService("cnpg"), mechanismLabel("cnpg"))
	}
}
