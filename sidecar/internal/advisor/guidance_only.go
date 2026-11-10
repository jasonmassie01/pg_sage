package advisor

import (
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// transformGuidanceOnly keeps configuration findings as guidance for a
// provider whose server parameters pg_sage cannot change through SQL or an
// adapter (Azure Cosmos DB for PostgreSQL): the finding loses its
// executable SQL and names the provider's command instead. Any ALTER
// SYSTEM statement counts, including ones that name no single setting.
func transformGuidanceOnly(findings []analyzer.Finding) []analyzer.Finding {
	out := make([]analyzer.Finding, 0, len(findings))
	for _, f := range findings {
		sql := strings.TrimSpace(f.RecommendedSQL)
		if !isAlterSystemStatement(sql) {
			out = append(out, f)
			continue
		}
		name := extractSettingName(sql)
		if name == "" {
			name = "<param>"
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

// isAlterSystemStatement reports whether sql starts with the keywords
// ALTER SYSTEM, whatever follows.
func isAlterSystemStatement(sql string) bool {
	words := strings.Fields(sql)
	return len(words) >= 2 && strings.EqualFold(words[0], "ALTER") &&
		strings.EqualFold(words[1], "SYSTEM")
}
