package advisor

import (
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"testing"
)

func TestTenantRollbackPreservesExplicitInverse(t *testing.T) {
	settings := []collector.PGSetting{{Name: "work_mem", Context: "user"}}
	for _, provider := range []string{"neon", "supabase"} {
		for _, inverse := range []string{"ALTER SYSTEM SET work_mem = '7MB'", "",
			"ALTER SYSTEM SET random_page_cost = 2", "ALTER SYSTEM RESET work_mem; SELECT 1"} {
			input := []analyzer.Finding{{RecommendedSQL: "ALTER SYSTEM SET work_mem = '16MB'",
				RollbackSQL: inverse}}
			got := TransformForCloud(input, provider, "db", settings)[0]
			if inverse == "ALTER SYSTEM SET work_mem = '7MB'" {
				if got.RollbackSQL != `ALTER DATABASE "db" SET work_mem = '7MB'` {
					t.Fatalf("prior override was lost: %q", got.RollbackSQL)
				}
			} else if got.RecommendedSQL != "" || got.RollbackSQL != "" {
				t.Fatal("missing or invalid inverse permits executable mutation")
			}
		}
	}
}
