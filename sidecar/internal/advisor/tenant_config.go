package advisor

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// Tenant providers permit SQL configuration only at supported scopes. Use the
// collected pg_settings context instead of guessing from a server parameter name.
func transformTenantConfig(
	findings []analyzer.Finding, provider, database string, settings [][]collector.PGSetting,
) []analyzer.Finding {
	contexts := make(map[string]string)
	for _, snapshot := range settings {
		for _, setting := range snapshot {
			contexts[setting.Name] = setting.Context
		}
	}
	out := make([]analyzer.Finding, 0, len(findings))
	for _, finding := range findings {
		sql := strings.TrimSpace(finding.RecommendedSQL)
		name := extractSettingName(sql)
		if name == "" {
			out = append(out, finding)
			continue
		}
		if contexts[name] != "user" || database == "" {
			finding.RecommendedSQL, finding.RollbackSQL = "", ""
			finding.Severity = "info"
			finding.Recommendation += tenantConfigGuidance(provider, name, contexts[name])
		} else if rollback := tenantInverse(finding.RollbackSQL, name, database); rollback != "" {
			finding.RecommendedSQL = "ALTER DATABASE " + sanitize.QuoteIdentifier(database) +
				sql[len("ALTER SYSTEM"):]
			finding.RollbackSQL = rollback
		} else {
			finding.RecommendedSQL, finding.RollbackSQL = "", ""
			finding.Severity = "info"
			finding.Recommendation += " (Capture the prior database setting and a valid inverse " +
				"before applying; RESET may discard an existing override.)"
		}
		out = append(out, finding)
	}
	return out
}

var tenantInverseSQL = regexp.MustCompile(`(?i)^ALTER SYSTEM (?:` +
	`RESET [a-z_][a-z0-9_.]*|SET [a-z_][a-z0-9_.]*\s+(?:=|TO)\s+` +
	`(?:'(?:[^']|'')*'|[a-z0-9_+.\-]+))\s*;?$`)

func tenantInverse(sql, name, database string) string {
	sql = strings.TrimSpace(sql)
	if !tenantInverseSQL.MatchString(sql) || extractSettingName(sql) != name {
		return ""
	}
	return "ALTER DATABASE " + sanitize.QuoteIdentifier(database) + sql[len("ALTER SYSTEM"):]
}

func tenantConfigGuidance(provider, name, settingContext string) string {
	if settingContext == "" {
		return fmt.Sprintf(" (Verify pg_settings.context and privileges for %s before applying.)", name)
	}
	if provider == "supabase" {
		if strings.HasPrefix(name, "auto_explain.") {
			return " (Supabase supports auto_explain settings for the privileged postgres role; " +
				"review ALTER ROLE scope and logging overhead before applying.)"
		}
		return " (Use a supported Supabase Postgres config API/CLI control for instance settings; " +
			"some privileged settings are permitted at role scope. Verify the setting and restart needs.)"
	}
	return " (Neon controls instance settings; use permitted session/database/role settings. " +
		"Consult Neon support for instance changes; availability is not guaranteed.)"
}
