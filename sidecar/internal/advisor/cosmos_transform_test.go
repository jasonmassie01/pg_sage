package advisor

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
)

// No concurrent access tests: TransformForCloud and IsManagedService are
// pure functions of their arguments.

func TestIsManagedServiceAzureCosmos(t *testing.T) {
	cases := map[string]bool{
		"azure-cosmos":  true,
		"AZURE-COSMOS ": true,
		"azure":         true,
		"":              false,
		"unknown":       false,
		"self-managed":  false,
		"postgres":      false,
		"cosmos":        false,
	}
	for platform, want := range cases {
		if got := IsManagedService(platform); got != want {
			t.Errorf("IsManagedService(%q) = %v, want %v", platform, got, want)
		}
	}
}

// Cosmos DB for PostgreSQL server parameters are guidance-only: the
// finding keeps no executable SQL and names the az cosmosdb command.
func TestTransformForCloudAzureCosmosDropsAlterSystem(t *testing.T) {
	in := []analyzer.Finding{{
		Category:       "memory_tuning",
		Severity:       "warning",
		Recommendation: "Raise work_mem.",
		RecommendedSQL: "ALTER SYSTEM SET work_mem='64MB'",
		RollbackSQL:    "ALTER SYSTEM RESET work_mem",
	}}
	out := TransformForCloud(in, "azure-cosmos", "app")
	if len(out) != 1 {
		t.Fatalf("got %d findings, want 1", len(out))
	}
	f := out[0]
	for _, sql := range []string{f.RecommendedSQL, f.RollbackSQL} {
		if strings.Contains(strings.ToUpper(sql), "ALTER SYSTEM") {
			t.Fatalf("cosmos finding keeps ALTER SYSTEM: %q", sql)
		}
	}
	if f.RecommendedSQL != "" || f.RollbackSQL != "" {
		t.Fatalf("cosmos parameters are guidance-only, got SQL %q / %q",
			f.RecommendedSQL, f.RollbackSQL)
	}
	if f.Severity != "info" {
		t.Errorf("severity = %q, want info", f.Severity)
	}
	for _, want := range []string{"Raise work_mem.", "az cosmosdb postgres configuration",
		"work_mem"} {
		if !strings.Contains(f.Recommendation, want) {
			t.Errorf("recommendation %q lacks %q", f.Recommendation, want)
		}
	}
	if strings.Contains(f.Recommendation, "flexible-server") {
		t.Errorf("cosmos guidance names the flexible-server command: %q", f.Recommendation)
	}
	if in[0].RecommendedSQL != "ALTER SYSTEM SET work_mem='64MB'" {
		t.Errorf("input finding was mutated: %q", in[0].RecommendedSQL)
	}
}

func TestTransformForCloudAzureCosmosCaseFolded(t *testing.T) {
	in := []analyzer.Finding{{RecommendedSQL: "ALTER SYSTEM SET work_mem = '64MB'"}}
	out := TransformForCloud(in, " AZURE-COSMOS ", "app")
	if len(out) != 1 || out[0].RecommendedSQL != "" {
		t.Fatalf("case-folded cosmos kept SQL: %+v", out)
	}
}

func TestTransformForCloudAzureCosmosKeepsNonConfigFindings(t *testing.T) {
	in := []analyzer.Finding{
		{Category: "missing_index", RecommendedSQL: "CREATE INDEX CONCURRENTLY i ON t (a)"},
		{Category: "info_only"},
	}
	out := TransformForCloud(in, "azure-cosmos", "app")
	if len(out) != 2 {
		t.Fatalf("got %d findings, want 2", len(out))
	}
	if out[0].RecommendedSQL != in[0].RecommendedSQL {
		t.Errorf("index SQL changed: %q", out[0].RecommendedSQL)
	}
	if out[1].RecommendedSQL != "" || out[1].Category != "info_only" {
		t.Errorf("empty finding changed: %+v", out[1])
	}
}

func TestTransformForCloudAzureCosmosEmpty(t *testing.T) {
	if out := TransformForCloud(nil, "azure-cosmos", "app"); len(out) != 0 {
		t.Fatalf("nil findings gave %d", len(out))
	}
}

// Aliases resolve through the provider registry: "aws" is RDS and "gcp"
// is Cloud SQL, so neither keeps ALTER SYSTEM. advisors-01 routes on the
// collected pg_settings context, so the user-context work_mem is given it.
func TestTransformForCloudAliasesUseCanonicalProfile(t *testing.T) {
	in := []analyzer.Finding{{RecommendedSQL: "ALTER SYSTEM SET work_mem = '64MB'"}}
	settings := []collector.PGSetting{{Name: "work_mem", Context: "user"}}
	out := TransformForCloud(in, "AWS", "app", settings)
	if out[0].RecommendedSQL != `ALTER DATABASE "app" SET work_mem = '64MB'` ||
		out[0].RollbackSQL != `ALTER DATABASE "app" RESET work_mem` {
		t.Fatalf("aws: %q / %q", out[0].RecommendedSQL, out[0].RollbackSQL)
	}
	in = []analyzer.Finding{{RecommendedSQL: "ALTER SYSTEM SET checkpoint_timeout = '15min'"}}
	out = TransformForCloud(in, "gcp", "app")
	if out[0].RecommendedSQL != "" || out[0].Severity != "info" ||
		!strings.Contains(out[0].Recommendation, "cloud-sql") {
		t.Fatalf("gcp restricted setting: %+v", out[0])
	}
}

// Invalid input: an ALTER SYSTEM statement the parser cannot read as one
// setting (RESET ALL, a missing value, odd spacing) still loses its SQL on
// Cosmos, so no ALTER SYSTEM survives the transform.
func TestTransformForCloudAzureCosmosUnparsableAlterSystem(t *testing.T) {
	for _, sql := range []string{
		"ALTER SYSTEM RESET ALL",
		"alter   system set work_mem",
		"  ALTER\tSYSTEM SET shared_buffers TO ;",
	} {
		in := []analyzer.Finding{{Severity: "warning", Recommendation: "Tune.",
			RecommendedSQL: sql, RollbackSQL: "ALTER SYSTEM RESET ALL"}}
		out := TransformForCloud(in, "azure-cosmos", "app")
		if len(out) != 1 {
			t.Fatalf("%q: got %d findings, want 1", sql, len(out))
		}
		f := out[0]
		if f.RecommendedSQL != "" || f.RollbackSQL != "" || f.Severity != "info" {
			t.Errorf("%q: kept SQL %q / %q severity %q", sql, f.RecommendedSQL,
				f.RollbackSQL, f.Severity)
		}
		if !strings.Contains(f.Recommendation, "az cosmosdb postgres configuration") {
			t.Errorf("%q: recommendation lacks the cosmos command: %q", sql, f.Recommendation)
		}
	}
}
