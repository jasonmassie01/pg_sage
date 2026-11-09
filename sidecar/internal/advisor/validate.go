package advisor

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/pgconf"
	"github.com/pg-sage/sidecar/internal/provider"
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

// RequiresRestart returns true if changing the setting needs a restart.
// The list is pgconf's, shared with the executor (G-P0-1).
func RequiresRestart(settingName string) bool {
	return pgconf.RequiresRestart(settingName)
}

// IsManagedService returns true if the platform is a managed cloud
// service where ALTER SYSTEM is unavailable (the provider registry).
func IsManagedService(platform string) bool {
	return provider.IsManaged(platform)
}

// TransformForCloud rewrites advisor findings for cloud platforms.
// On managed services: ALTER SYSTEM SET → ALTER DATABASE dbname SET,
// and restart-requiring GUCs are downgraded to info-only (no SQL).
// Tenant providers keep only database-level settings, and guidance-only
// providers keep no executable SQL.
func TransformForCloud(
	findings []analyzer.Finding,
	platform, dbName string,
	settings ...[]collector.PGSetting,
) []analyzer.Finding {
	profile, ok := provider.Lookup(platform)
	if !ok || !profile.Managed {
		return findings
	}
	switch profile.ConfigMechanism {
	case provider.ConfigTenant:
		return transformTenantConfig(findings, profile.Name, dbName, settings)
	case provider.ConfigGuidanceOnly:
		return transformGuidanceOnly(findings)
	}
	out := make([]analyzer.Finding, 0, len(findings))
	for _, f := range findings {
		out = append(out, transformManagedFinding(f, profile.Name, dbName))
	}
	return out
}

// transformManagedFinding rewrites one finding for a managed platform.
func transformManagedFinding(f analyzer.Finding, platform, dbName string) analyzer.Finding {
	sql := strings.TrimSpace(f.RecommendedSQL)
	if sql == "" {
		return f
	}
	settingName := extractSettingName(sql)

	// Drop executable SQL for restart-requiring or platform-restricted
	// GUCs — managed services control these via their console, not SQL.
	if settingName != "" && RequiresRestart(settingName) {
		f = withManagedIntent(f, platform, sql, "requires a restart")
		f.RecommendedSQL = ""
		f.RollbackSQL = ""
		f.Recommendation += fmt.Sprintf(
			" (Note: %s requires a restart and must "+
				"be changed via %s console, not SQL.)",
			settingName, platform,
		)
		f.Severity = "info"
		return f
	}
	if settingName != "" && restrictedSettings[platform][settingName] {
		f = withManagedIntent(f, platform, sql,
			"not adjustable through SQL on "+platform)
		f.RecommendedSQL = ""
		f.RollbackSQL = ""
		f.Recommendation += fmt.Sprintf(
			" (Note: %s is not adjustable via SQL "+
				"on %s — change via platform console.)",
			settingName, platform,
		)
		f.Severity = "info"
		return f
	}
	return rewriteAlterSystem(f, sql, settingName, dbName)
}

// rewriteAlterSystem turns ALTER SYSTEM SET/RESET into ALTER DATABASE.
func rewriteAlterSystem(f analyzer.Finding, sql, settingName, dbName string) analyzer.Finding {
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
	return f
}

// extractSettingName parses the GUC name from ALTER SYSTEM SET name
// or ALTER SYSTEM RESET name statements.
func extractSettingName(sql string) string {
	stmt, ok := pgconf.ParseAlterSystem(sql)
	if !ok {
		return ""
	}
	return stmt.Name
}
