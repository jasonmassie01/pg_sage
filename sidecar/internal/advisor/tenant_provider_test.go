package advisor

import (
	"reflect"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
)

func TestTenantCloudTransformUsesActualSettingContext(t *testing.T) {
	settings := []collector.PGSetting{
		{Name: "work_mem", Context: "user"},
		{Name: "max_slot_wal_keep_size", Context: "sighup"},
		{Name: "auto_explain.log_min_duration", Context: "superuser"},
	}
	for _, provider := range []string{"neon", "supabase"} {
		t.Run(provider, func(t *testing.T) {
			findings := []analyzer.Finding{
				{RecommendedSQL: "ALTER SYSTEM SET work_mem = '16MB'",
					RollbackSQL: "ALTER SYSTEM RESET work_mem"},
				{RecommendedSQL: "ALTER SYSTEM SET max_slot_wal_keep_size = '1GB'"},
				{RecommendedSQL: "ALTER SYSTEM SET unknown_guc = '1'"},
				{RecommendedSQL: "ALTER SYSTEM SET auto_explain.log_min_duration = '1s'"},
			}
			got := TransformForCloud(findings, provider, `test"db`, settings)
			if got[0].RecommendedSQL != `ALTER DATABASE "test""db" SET work_mem = '16MB'` ||
				got[0].RollbackSQL != `ALTER DATABASE "test""db" RESET work_mem` {
				t.Fatalf("user-context conversion: %+v", got[0])
			}
			for _, f := range got[1:] {
				if f.RecommendedSQL != "" || f.RollbackSQL != "" || f.Recommendation == "" {
					t.Fatalf("unverified scope emitted executable SQL: %+v", f)
				}
			}
			if provider == "supabase" && !strings.Contains(got[3].Recommendation, "role") {
				t.Fatal("Supabase delegated role setting guidance missing")
			}
			if findings[0].RecommendedSQL != "ALTER SYSTEM SET work_mem = '16MB'" {
				t.Fatal("transform mutated input findings")
			}
		})
	}
}

func TestTenantCloudTransformMissingSettingsFailsClosed(t *testing.T) {
	for _, provider := range []string{"neon", "supabase"} {
		if !IsManagedService(provider) {
			t.Fatalf("%s not classified as managed", provider)
		}
		if got := TransformForCloud(nil, provider, "db"); len(got) != 0 {
			t.Fatalf("nil findings expanded: %+v", got)
		}
		input := []analyzer.Finding{{RecommendedSQL: "ALTER SYSTEM SET work_mem = '16MB'"}}
		got := TransformForCloud(input, provider, "db")
		if got[0].RecommendedSQL != "" || got[0].Recommendation == "" {
			t.Fatalf("missing pg_settings context should be visible: %+v", got[0])
		}
	}
}

func TestTenantCloudTransformResetAndUnrelatedActions(t *testing.T) {
	settings := []collector.PGSetting{{Name: "work_mem", Context: "user"}}
	for _, provider := range []string{"neon", "supabase"} {
		input := []analyzer.Finding{
			{RecommendedSQL: "  alter system reset work_mem;  ",
				RollbackSQL: "ALTER SYSTEM SET work_mem = '4MB'"},
			{RecommendedSQL: "ANALYZE public.orders", RollbackSQL: "existing rollback"},
		}
		got := TransformForCloud(input, provider, "db", settings)
		if got[0].RecommendedSQL != `ALTER DATABASE "db" reset work_mem;` {
			t.Fatalf("reset transformation: %+v", got[0])
		}
		if !reflect.DeepEqual(got[1], input[1]) {
			t.Fatalf("unrelated action modified: %+v", got[1])
		}
		missingDB := TransformForCloud(input[:1], provider, "", settings)
		if missingDB[0].RecommendedSQL != "" || missingDB[0].Recommendation == "" {
			t.Fatalf("missing database emits executable SQL: %+v", missingDB[0])
		}
	}
}

// No concurrent test: the transformation only reads arguments and copies findings.
// Live ALTER DATABASE effects and privilege failures belong to the provider harness.
