package advisor

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// settingsByName indexes the collected pg_settings rows by name; a later
// snapshot's row wins.
func settingsByName(snapshots [][]collector.PGSetting) map[string]collector.PGSetting {
	known := make(map[string]collector.PGSetting)
	for _, snapshot := range snapshots {
		for _, setting := range snapshot {
			known[setting.Name] = setting
		}
	}
	return known
}

// routeManagedSetting is TransformForCloud for one finding: it decides by
// the setting's pg_settings context whether ALTER DATABASE can carry the
// change. Anything it cannot confirm loses its SQL (fail closed).
func routeManagedSetting(f analyzer.Finding, platform, database string,
	known map[string]collector.PGSetting) analyzer.Finding {
	sql := strings.TrimSpace(f.RecommendedSQL)
	name := extractSettingName(sql)
	if name == "" {
		return unparsedSetting(f, platform, sql)
	}
	setting, collected := known[name]
	switch {
	case RequiresRestart(name) || setting.Context == "postmaster":
		return instanceSetting(f, platform, sql, name, "postmaster", fmt.Sprintf(
			" (Note: %s requires a restart and must be changed via %s console, not SQL.)",
			name, platform), "requires a restart")
	case setting.Context == "sighup":
		return instanceSetting(f, platform, sql, name, "sighup", fmt.Sprintf(
			" (Note: %s is not adjustable via SQL on %s: it has sighup context, which "+
				"ALTER DATABASE cannot set; change it in %s.)", name, platform,
			settingsSurface(platform)), "not adjustable through SQL on "+platform)
	case !collected || setting.Context == "":
		return instanceSetting(f, platform, sql, name, "", fmt.Sprintf(
			" (Note: pg_sage has not confirmed the pg_settings context of %s on %s, so "+
				"it does not run this change; verify the context and privileges first.)",
			name, platform), "its pg_settings context is unconfirmed")
	case setting.Context == "superuser" && !setting.SetPrivilege:
		return instanceSetting(f, platform, sql, name, setting.Context, fmt.Sprintf(
			" (Note: %s has superuser context and the connected role lacks SET "+
				"privilege on it, so ALTER DATABASE would be refused on %s.)", name, platform),
			"the connected role may not SET it")
	case setting.Context != "user" && setting.Context != "superuser":
		return instanceSetting(f, platform, sql, name, setting.Context, fmt.Sprintf(
			" (Note: %s has %s context, which ALTER DATABASE cannot set on %s.)",
			name, setting.Context, platform), setting.Context+" context")
	case strings.TrimSpace(database) == "":
		return instanceSetting(f, platform, sql, name, setting.Context, fmt.Sprintf(
			" (Note: no database name is known to scope %s with ALTER DATABASE on %s.)",
			name, platform), "no database to scope it to")
	}
	return databaseScoped(f, platform, sql, name, setting.Context, database)
}

// unparsedSetting leaves SQL that is not ALTER SYSTEM alone. An ALTER SYSTEM
// the parser cannot read names no setting to route, and no managed provider
// accepts ALTER SYSTEM, so it loses its SQL (fail closed).
func unparsedSetting(f analyzer.Finding, platform, sql string) analyzer.Finding {
	if !strings.HasPrefix(strings.ToUpper(sql), "ALTER SYSTEM") {
		return f
	}
	f.RecommendedSQL, f.RollbackSQL, f.Severity = "", "", "info"
	f.Recommendation += fmt.Sprintf(" (Note: %s does not accept ALTER SYSTEM, and pg_sage "+
		"could not read which setting this statement changes; review it manually.)", platform)
	return f
}

// databaseScoped rewrites ALTER SYSTEM SET/RESET into ALTER DATABASE. The
// rollback is a hint: a valid caller inverse converted to database scope,
// else a RESET. The executor captures the prior database setting at apply
// time and replaces the hint with the rollback that restores it.
func databaseScoped(f analyzer.Finding, platform, sql, name, settingContext,
	database string) analyzer.Finding {
	upper := strings.ToUpper(sql)
	reset := strings.HasPrefix(upper, "ALTER SYSTEM RESET ")
	if !reset && !strings.HasPrefix(upper, "ALTER SYSTEM SET ") {
		return advisorySetting(f, platform, name, settingContext, fmt.Sprintf(
			" (Note: pg_sage could not rewrite this %s change to database scope.)", name))
	}
	f.RecommendedSQL = "ALTER DATABASE " + sanitize.QuoteIdentifier(database) +
		sql[len("ALTER SYSTEM"):]
	rollback := databaseInverse(f.RollbackSQL, name, database)
	if rollback == "" && !reset {
		rollback = "ALTER DATABASE " + sanitize.QuoteIdentifier(database) + " RESET " + name
	}
	f.RollbackSQL = rollback
	return f
}

// instanceSetting handles a change ALTER DATABASE cannot carry: the typed
// managed change where the provider has one (always operator-approved; a
// parameter group or flag can set any of these settings), then the
// advisory downgrade.
func instanceSetting(f analyzer.Finding, platform, sql, name, settingContext, note,
	why string) analyzer.Finding {
	return advisorySetting(withManagedIntent(f, platform, sql, why), platform, name,
		settingContext, note)
}

// advisorySetting drops the finding's SQL and appends guidance; a tenant
// provider's own guidance replaces the generic note.
func advisorySetting(f analyzer.Finding, platform, name, settingContext,
	note string) analyzer.Finding {
	f.RecommendedSQL, f.RollbackSQL = "", ""
	f.Severity = "info"
	if tenantProvider(platform) {
		note = tenantConfigGuidance(platform, name, settingContext)
	}
	f.Recommendation += note
	return f
}

func settingsSurface(platform string) string {
	if surface, ok := platformSettingsSurface[platform]; ok {
		return surface
	}
	return "the provider's console"
}

// tenantProvider reports a provider that hosts databases in a shared
// instance it controls (Neon, Supabase, Lakebase).
func tenantProvider(platform string) bool {
	return platform == "neon" || platform == "supabase" || platform == "lakebase"
}

var inverseSQL = regexp.MustCompile(`(?i)^ALTER SYSTEM (?:` +
	`RESET [a-z_][a-z0-9_.]*|SET [a-z_][a-z0-9_.]*\s+(?:=|TO)\s+` +
	`(?:'(?:[^']|'')*'|[a-z0-9_+.\-]+))\s*;?$`)

// databaseInverse converts a caller's single-statement ALTER SYSTEM inverse
// of the same setting to database scope; anything else gives "".
func databaseInverse(sql, name, database string) string {
	sql = strings.TrimSpace(sql)
	if !inverseSQL.MatchString(sql) || extractSettingName(sql) != name {
		return ""
	}
	return "ALTER DATABASE " + sanitize.QuoteIdentifier(database) + sql[len("ALTER SYSTEM"):]
}

// tenantConfigGuidance is a tenant provider's advisory note; it names the
// setting and the provider, so a withheld change says what it was and why.
func tenantConfigGuidance(provider, name, settingContext string) string {
	return fmt.Sprintf(" (%s on %s: %s)", name, provider,
		tenantGuidanceText(provider, name, settingContext))
}

func tenantGuidanceText(provider, name, settingContext string) string {
	if settingContext == "" {
		return "Verify pg_settings.context and privileges before applying."
	}
	if provider == "lakebase" {
		return "Lakebase manages instance settings; only permitted user-context " +
			"session/database/role settings can be changed. ALTER SYSTEM and instance " +
			"changes are unavailable."
	}
	if provider == "supabase" {
		if strings.HasPrefix(name, "auto_explain.") {
			return "Supabase supports auto_explain settings for the privileged postgres " +
				"role; review ALTER ROLE scope and logging overhead before applying."
		}
		return "Use a supported Supabase Postgres config API/CLI control for instance " +
			"settings; some privileged settings are permitted at role scope. Verify the " +
			"setting and restart needs."
	}
	return "Neon controls instance settings; use permitted session/database/role settings. " +
		"Consult Neon support for instance changes; availability is not guaranteed."
}
