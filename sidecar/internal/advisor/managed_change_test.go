package advisor

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/managedparam"
)

// advisors-03: Azure flexible server and AlloyDB findings that SQL cannot
// carry get a typed managed-change intent (operator-approved), like RDS,
// Aurora and Cloud SQL; Cosmos DB for PostgreSQL and single server do not.
//
// No concurrent access tests: withManagedIntent is pure and clones Detail
// (TestTransformForCloudConcurrent covers the gate under parallel use).

// CHECK-06: with cloud_environment azure, a max_wal_size finding carries
// detail.managed_change.provider == "azure" and no RecommendedSQL.
func TestMaxWALSizeOnAzureCarriesAManagedIntent(t *testing.T) {
	cases := map[string]string{"azure": "server parameter", "azure-flexible": "server parameter",
		"alloydb": "database flag"}
	for platform, label := range cases {
		f := gateMatrix(t, matrixFinding("ALTER SYSTEM SET max_wal_size = '4GB'"), platform,
			"app", sighupRows())
		if f.RecommendedSQL != "" || f.RollbackSQL != "" {
			t.Fatalf("%s: SQL survived: %q / %q", platform, f.RecommendedSQL, f.RollbackSQL)
		}
		raw, ok := f.Detail[managedparam.DetailKey].(map[string]any)
		want := managedparam.NormalizeProvider(platform)
		if !ok || raw["provider"] != want || raw["parameter"] != "max_wal_size" ||
			raw["value"] != "4GB" {
			t.Fatalf("%s: managed_change = %v", platform, f.Detail[managedparam.DetailKey])
		}
		approval, _ := f.Detail[analyzer.DetailApprovalRequired].(string)
		if !strings.Contains(approval, label) || !strings.Contains(approval, "operator") {
			t.Fatalf("%s: approval = %q, want the %s label", platform, approval, label)
		}
	}
}

// The gate routes by the canonical profile name, so the retired
// azure-single alias (never a detected or configurable provider) reads as
// azure here; managedparam refuses the raw alias itself.
func TestManagedIntentSkipsGuidanceOnlyAzureFlavours(t *testing.T) {
	for _, platform := range []string{"azure-cosmos", "AZURE-COSMOS"} {
		f := gateMatrix(t, matrixFinding("ALTER SYSTEM SET max_wal_size = '4GB'"), platform,
			"app", sighupRows())
		if _, ok := f.Detail[managedparam.DetailKey]; ok || f.RecommendedSQL != "" {
			t.Fatalf("%s: must stay guidance: %+v", platform, f)
		}
	}
}

func TestMechanismLabel(t *testing.T) {
	cases := map[string]string{"rds": "parameter group", "aurora": "parameter group",
		"cloud-sql": "database flag", "gcp": "database flag", "alloydb": "database flag",
		"azure": "server parameter", "azure-flexible": "server parameter"}
	for platform, want := range cases {
		if got := mechanismLabel(platform); got != want {
			t.Errorf("mechanismLabel(%q) = %q, want %q", platform, got, want)
		}
	}
}

// A RESET, an empty value or a value no command may carry gives no intent
// on the new providers either.
func TestManagedIntentRefusesUnusableStatements(t *testing.T) {
	for _, sql := range []string{"ALTER SYSTEM RESET max_wal_size",
		"ALTER SYSTEM SET max_wal_size = ''", "ALTER SYSTEM SET max_wal_size = '4GB; x'"} {
		f := withManagedIntent(matrixFinding(sql), "azure", sql, "sighup")
		if _, ok := f.Detail[managedparam.DetailKey]; ok {
			t.Errorf("%q: intent recorded: %v", sql, f.Detail)
		}
	}
}
