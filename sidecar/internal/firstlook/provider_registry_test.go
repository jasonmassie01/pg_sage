package firstlook

import (
	"strings"
	"testing"
)

// No concurrent access tests: ExtensionCapabilities is a pure function.

// Cosmos DB for PostgreSQL gets its own preload steps (the az cosmosdb
// command), not the flexible-server command; aliases resolve to their
// canonical provider's steps.
func TestPreloadStepsFromProviderRegistry(t *testing.T) {
	e := allReady()
	e.StatStatementsInstalled, e.StatStatementsLoaded = false, boolp(false)
	e.HypoPGInstalled, e.HypoPGAvailable, e.AutoExplainLoaded = false, false, false
	cases := map[string]struct{ want, not []string }{
		"azure-cosmos": {[]string{"az cosmosdb postgres configuration", "PG_STAT_STATEMENTS",
			"azure.extensions"}, []string{"flexible-server", "ALTER SYSTEM"}},
		" AZURE-COSMOS ": {[]string{"az cosmosdb postgres configuration"}, nil},
		"aws":            {[]string{"DB parameter group", "reboot"}, []string{"ALTER SYSTEM"}},
		"gcp":            {[]string{"Cloud SQL preloads"}, []string{"ALTER SYSTEM"}},
		"neon":           {[]string{"pg_stat_statements is preloaded on neon."}, nil},
		"self-managed":   {[]string{"ALTER SYSTEM SET shared_preload_libraries"}, nil},
		"":               {[]string{"ALTER SYSTEM SET shared_preload_libraries"}, nil},
	}
	for provider, tc := range cases {
		caps, _ := ExtensionCapabilities(e, provider)
		steps := capability(caps, CapStatStatements).Steps
		joined := strings.Join(steps, "\n")
		if len(steps) == 0 || steps[len(steps)-1] !=
			"CREATE EXTENSION IF NOT EXISTS pg_stat_statements;" {
			t.Errorf("%q: steps %q do not end with CREATE EXTENSION", provider, steps)
		}
		for _, want := range tc.want {
			if !strings.Contains(joined, want) {
				t.Errorf("%q: steps %q lack %q", provider, joined, want)
			}
		}
		for _, not := range tc.not {
			if strings.Contains(joined, not) {
				t.Errorf("%q: steps %q contain %q", provider, joined, not)
			}
		}
	}
	caps, _ := ExtensionCapabilities(e, "azure-cosmos")
	if h := strings.Join(capability(caps, CapHypoPG).Steps, " "); !strings.Contains(h,
		"azure.extensions") {
		t.Errorf("cosmos hypopg steps %q", h)
	}
	a := capability(caps, CapAutoExplain).Steps
	if len(a) == 0 || !strings.Contains(a[len(a)-1], "auto_explain.log_min_duration") ||
		strings.Contains(strings.Join(a, " "), "CREATE EXTENSION") {
		t.Errorf("cosmos auto_explain steps %q", a)
	}
}
