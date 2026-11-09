package advisor

import (
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/pgconf"
)

// RequiresRestart returns true if changing the setting needs a restart.
// The list is pgconf's, shared with the executor (G-P0-1).
func RequiresRestart(settingName string) bool {
	return pgconf.RequiresRestart(settingName)
}

// IsManagedService returns true if the platform is a managed cloud
// service where ALTER SYSTEM is unavailable.
func IsManagedService(platform string) bool {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "rds", "aurora", "cloud-sql", "alloydb", "azure", "neon", "supabase", "lakebase":
		return true
	}
	return false
}

// TransformForCloud rewrites setting findings for a managed provider,
// where ALTER SYSTEM is unavailable. It routes each ALTER SYSTEM on the
// setting's pg_settings context, collected in settings (advisors-01):
//   - user, or superuser with the connected role's SET privilege:
//     ALTER DATABASE <db> SET; the executor captures the prior value and
//     builds the rollback that restores it.
//   - sighup and postmaster (and the restart list): no SQL, since
//     PostgreSQL refuses ALTER DATABASE SET for them (55P02); a typed
//     managed intent where the provider has one, guidance otherwise.
//   - unknown or missing context, or no database name: advisory.
//
// Self-managed platforms keep ALTER SYSTEM unchanged.
func TransformForCloud(
	findings []analyzer.Finding,
	platform, dbName string,
	settings ...[]collector.PGSetting,
) []analyzer.Finding {
	platform = strings.ToLower(strings.TrimSpace(platform))
	if !IsManagedService(platform) {
		return findings
	}
	known := settingsByName(settings)
	out := make([]analyzer.Finding, 0, len(findings))
	for _, f := range findings {
		out = append(out, routeManagedSetting(f, platform, dbName, known))
	}
	return out
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
