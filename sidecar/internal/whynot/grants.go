package whynot

import (
	prov "github.com/pg-sage/sidecar/internal/provider"
	"github.com/pg-sage/sidecar/internal/sqlident"
)

// grantRunner names who may run a GRANT of a predefined role on each
// managed provider: the provider's administrator role, since nobody there
// is a superuser. Self-managed PostgreSQL and unknown providers need a
// superuser.
var grantRunner = map[string]string{
	prov.RDS:      "a member of rds_superuser",
	prov.Aurora:   "a member of rds_superuser",
	prov.CloudSQL: "a member of cloudsqlsuperuser",
	prov.AlloyDB:  "a member of alloydbsuperuser",
	prov.Azure:    "a member of azure_pg_admin",
	prov.Supabase: "the postgres role",
	prov.Neon:     "the neon_superuser role (the project owner)",
	prov.Lakebase: "the instance's databricks_superuser role",
}

// runnerFor names who runs a grant on provider.
func runnerFor(provider string) string {
	if runner, ok := grantRunner[prov.Canonical(provider)]; ok {
		return runner
	}
	return "a superuser"
}

// statsGrantFix is the exact GRANT that shows the role every role's query
// text, with who runs it on the detected provider.
func statsGrantFix(role, provider string) *Fix {
	if role == "" {
		role = "sage_agent"
	}
	return &Fix{Kind: FixGrantSQL,
		Value: "GRANT pg_read_all_stats TO " + sqlident.Role(role) + ";",
		Note:  "Run it as " + runnerFor(provider) + "; it applies on pg_sage's next read."}
}

// extensionFix creates pg_stat_statements in the database.
func extensionFix(provider string) *Fix {
	note := "Run it as " + runnerFor(provider) + "; the library must be in " +
		"shared_preload_libraries."
	if steps := prov.PreloadSteps(provider, "pg_stat_statements"); len(steps) > 0 {
		note = "Run it as " + runnerFor(provider) + " after: " + steps[0]
	}
	return &Fix{Kind: FixGrantSQL, Value: "CREATE EXTENSION IF NOT EXISTS pg_stat_statements;",
		Note: note}
}
