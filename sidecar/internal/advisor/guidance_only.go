package advisor

import (
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// transformGuidanceOnly keeps configuration findings as guidance for a
// provider whose server parameters pg_sage cannot change through SQL or an
// adapter (Azure Cosmos DB for PostgreSQL): the finding loses its
// executable SQL and names the provider's command instead.
func transformGuidanceOnly(findings []analyzer.Finding) []analyzer.Finding {
	out := make([]analyzer.Finding, 0, len(findings))
	for _, f := range findings {
		name := extractSettingName(strings.TrimSpace(f.RecommendedSQL))
		if name == "" {
			out = append(out, f)
			continue
		}
		f.RecommendedSQL, f.RollbackSQL = "", ""
		f.Severity = "info"
		f.Recommendation += " (Note: Azure Cosmos DB for PostgreSQL server parameters " +
			"are guidance-only: set " + name + " with `az cosmosdb postgres configuration " +
			"coordinator update --name " + name + " --value <value>`; ALTER SYSTEM is " +
			"unavailable.)"
		out = append(out, f)
	}
	return out
}
