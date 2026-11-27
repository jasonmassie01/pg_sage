package advisor

import (
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/pgconf"
	"github.com/pg-sage/sidecar/internal/provider"
)

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
// A guidance-only provider (the registry's ConfigGuidanceOnly, such as
// Azure Cosmos DB for PostgreSQL) keeps no executable SQL (clouds-11).
// Self-managed platforms keep ALTER SYSTEM unchanged.
func TransformForCloud(
	findings []analyzer.Finding,
	platform, dbName string,
	settings ...[]collector.PGSetting,
) []analyzer.Finding {
	profile, ok := provider.Lookup(platform)
	if !ok || !profile.Managed {
		return findings
	}
	if profile.ConfigMechanism == provider.ConfigGuidanceOnly {
		return transformGuidanceOnly(findings)
	}
	known := settingsByName(settings)
	out := make([]analyzer.Finding, 0, len(findings))
	for _, f := range findings {
		out = append(out, routeManagedSetting(f, profile.Name, dbName, known))
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
