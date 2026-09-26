package advisor

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// Known managed service platform restrictions.
var restrictedSettings = map[string]map[string]bool{
	"cloud-sql": {
		"wal_level": true, "full_page_writes": true,
		"shared_buffers": true, "checkpoint_timeout": true,
	},
	"alloydb": {
		"wal_level": true, "full_page_writes": true,
		"shared_buffers": true, "checkpoint_timeout": true,
	},
	"aurora": {
		"wal_level": true, "full_page_writes": true,
		"max_wal_size": true, "min_wal_size": true,
		"checkpoint_timeout": true,
	},
	"rds": {
		"wal_level": true, "full_page_writes": true,
		"max_wal_size": true, "min_wal_size": true,
		"checkpoint_timeout": true,
	},
}

// restartRequired lists GUCs that need a restart.
var restartRequired = map[string]bool{
	"max_connections": true,
	"shared_buffers":  true,
	"huge_pages":      true,
	"wal_level":       true,
	"max_wal_senders": true,
	"wal_buffers":     true,
}

// RequiresRestart returns true if changing the setting needs a restart.
func RequiresRestart(settingName string) bool {
	return restartRequired[settingName]
}

// IsManagedService returns true if the platform is a managed cloud
// service where ALTER SYSTEM is unavailable.
func IsManagedService(platform string) bool {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "rds", "aurora", "cloud-sql", "alloydb", "azure", "neon", "supabase":
		return true
	}
	return false
}

// TransformForCloud rewrites advisor findings for cloud platforms.
// On managed services: ALTER SYSTEM SET → ALTER DATABASE dbname SET,
// and restart-requiring GUCs are downgraded to info-only (no SQL).
func TransformForCloud(
	findings []analyzer.Finding,
	platform, dbName string,
	settings ...[]collector.PGSetting,
) []analyzer.Finding {
	platform = strings.ToLower(strings.TrimSpace(platform))
	if platform == "neon" || platform == "supabase" {
		return transformTenantConfig(findings, platform, dbName, settings)
	}
	if !IsManagedService(platform) {
		return findings
	}

	out := make([]analyzer.Finding, 0, len(findings))
	for _, f := range findings {
		sql := strings.TrimSpace(f.RecommendedSQL)
		if sql == "" {
			out = append(out, f)
			continue
		}

		settingName := extractSettingName(sql)

		// Drop executable SQL for restart-requiring or
		// platform-restricted GUCs — managed services
		// control these via their console, not SQL.
		if settingName != "" && RequiresRestart(settingName) {
			f.RecommendedSQL = ""
			f.RollbackSQL = ""
			f.Recommendation += fmt.Sprintf(
				" (Note: %s requires a restart and must "+
					"be changed via %s console, not SQL.)",
				settingName, platform,
			)
			f.Severity = "info"
			out = append(out, f)
			continue
		}
		if settingName != "" {
			if restricted, ok := restrictedSettings[platform]; ok {
				if restricted[settingName] {
					f.RecommendedSQL = ""
					f.RollbackSQL = ""
					f.Recommendation += fmt.Sprintf(
						" (Note: %s is not adjustable via SQL "+
							"on %s — change via platform console.)",
						settingName, platform,
					)
					f.Severity = "info"
					out = append(out, f)
					continue
				}
			}
		}

		// Rewrite ALTER SYSTEM → ALTER DATABASE.
		upper := strings.ToUpper(sql)
		quoted := sanitize.QuoteIdentifier(dbName)
		if strings.HasPrefix(upper, "ALTER SYSTEM SET ") {
			rest := sql[len("ALTER SYSTEM SET "):]
			f.RecommendedSQL = fmt.Sprintf(
				"ALTER DATABASE %s SET %s", quoted, rest,
			)
			if settingName != "" {
				f.RollbackSQL = fmt.Sprintf(
					"ALTER DATABASE %s RESET %s",
					quoted, settingName,
				)
			}
		} else if strings.HasPrefix(upper, "ALTER SYSTEM RESET ") {
			rest := sql[len("ALTER SYSTEM RESET "):]
			f.RecommendedSQL = fmt.Sprintf(
				"ALTER DATABASE %s RESET %s", quoted, rest,
			)
		}

		out = append(out, f)
	}
	return out
}

// extractSettingName parses the GUC name from ALTER SYSTEM SET name
// or ALTER SYSTEM RESET name statements.
func extractSettingName(sql string) string {
	upper := strings.ToUpper(strings.TrimSpace(sql))
	var rest string
	switch {
	case strings.HasPrefix(upper, "ALTER SYSTEM SET "):
		rest = strings.TrimSpace(sql[len("ALTER SYSTEM SET "):])
	case strings.HasPrefix(upper, "ALTER SYSTEM RESET "):
		rest = strings.TrimSpace(sql[len("ALTER SYSTEM RESET "):])
	default:
		return ""
	}
	// Setting name is the first token.
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(fields[0], ";"))
}
