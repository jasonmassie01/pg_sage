package advisor

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
)

// A valid caller inverse becomes the database-scoped rollback hint; a
// missing or invalid one no longer blocks the change (advisors-01): the
// executor captures the prior database setting itself and replaces the
// hint with the rollback that restores it.
func TestTenantRollbackPreservesExplicitInverse(t *testing.T) {
	settings := []collector.PGSetting{{Name: "work_mem", Context: "user"}}
	for _, provider := range []string{"neon", "supabase"} {
		for inverse, want := range map[string]string{
			"ALTER SYSTEM SET work_mem = '7MB'":     `ALTER DATABASE "db" SET work_mem = '7MB'`,
			"":                                      `ALTER DATABASE "db" RESET work_mem`,
			"ALTER SYSTEM SET random_page_cost = 2": `ALTER DATABASE "db" RESET work_mem`,
			"ALTER SYSTEM RESET work_mem; SELECT 1": `ALTER DATABASE "db" RESET work_mem`,
		} {
			input := []analyzer.Finding{{RecommendedSQL: "ALTER SYSTEM SET work_mem = '16MB'",
				RollbackSQL: inverse}}
			got := TransformForCloud(input, provider, "db", settings)[0]
			if got.RecommendedSQL != `ALTER DATABASE "db" SET work_mem = '16MB'` {
				t.Fatalf("%s inverse %q: change not executable: %q", provider, inverse,
					got.RecommendedSQL)
			}
			if got.RollbackSQL != want {
				t.Fatalf("%s inverse %q: rollback %q, want %q", provider, inverse,
					got.RollbackSQL, want)
			}
		}
	}
}
