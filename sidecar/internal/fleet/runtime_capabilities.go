package fleet

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/tuner"
)

func collectRuntimeEvidence(ctx context.Context, pool *pgxpool.Pool, caps *ProviderCapabilities) {
	defer applyProviderCapabilityLimits(caps)
	for name := range caps.Extensions {
		caps.Extensions[name] = "unknown"
	}
	caps.Extensions["vector"] = "unknown"
	if pool == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	collectExtensionEvidence(ctx, pool, caps)
	collectHintReadiness(ctx, pool, caps)
	collectStatsPrivilege(ctx, pool, caps)
	collectMaintainPrivilege(ctx, pool, caps)
}

// maintainerRoles are the roles whose members may ANALYZE and VACUUM every
// table: the managed providers' admin roles and PostgreSQL 17's
// pg_maintain. A role a server lacks simply matches nothing.
var maintainerRoles = []string{
	"rds_superuser", "cloudsqlsuperuser", "alloydbsuperuser", "azure_pg_admin", "pg_maintain",
}

// maintainPrivilegeSQL passes role OIDs to pg_has_role, so a listed role
// that does not exist on this server version raises no error. USAGE (not
// MEMBER) asks whether the role holds the privileges without SET ROLE, so a
// NOINHERIT member is not reported as able to maintain every table.
const maintainPrivilegeSQL = `SELECT r.rolsuper OR EXISTS (
	SELECT 1 FROM pg_catalog.pg_roles g
	 WHERE g.rolname = ANY($1) AND pg_has_role(current_user, g.oid, 'USAGE'))
  FROM pg_catalog.pg_roles r WHERE r.rolname = current_user`

const maintainProbeFailed = "maintenance privilege probe failed; check role access"

// collectMaintainPrivilege sets the analyze permission: ok for a superuser
// or a member of a maintainer role, limited (checked per table at
// execution) otherwise. A failed probe leaves it unknown.
func collectMaintainPrivilege(
	ctx context.Context, pool *pgxpool.Pool, caps *ProviderCapabilities,
) {
	var maintains bool
	err := pool.QueryRow(ctx, maintainPrivilegeSQL, maintainerRoles).Scan(&maintains)
	if err != nil {
		caps.Limitations = append(caps.Limitations, maintainProbeFailed)
		return
	}
	status := CapabilityStatus{Status: "limited", Reason: "role is not a superuser or " +
		"maintainer; table ANALYZE permission checked at execution"}
	if maintains {
		status = CapabilityStatus{Status: "ok",
			Reason: "role is a superuser or holds a maintainer role's privileges"}
	}
	caps.Permissions["analyze"] = status
}

// collectHintReadiness reads, catalog only, whether pg_hint_plan hints
// reach application sessions, and names the missing prerequisite as a
// limitation. A loaded module with enable_hint_table off ignores
// hint_plan.hints entirely.
func collectHintReadiness(ctx context.Context, pool *pgxpool.Pool, caps *ProviderCapabilities) {
	switch caps.Extensions["pg_hint_plan"] {
	case "not_installed", "unknown", "":
		return
	}
	avail, err := tuner.ProbeHintReadiness(ctx, pool)
	if err != nil {
		caps.Limitations = append(caps.Limitations,
			"pg_hint_plan readiness probe failed; check catalog access")
	}
	caps.HintPlan = avail
	if l := hintPlanLimitation(avail); l != "" {
		caps.Limitations = append(caps.Limitations, l)
	}
}

// hintPlanLimitation is the limitation line for facts without application
// effect ("" when hints take effect or nothing was probed).
func hintPlanLimitation(a *tuner.HintPlanAvailability) string {
	if a == nil || a.ApplicationEffect {
		return ""
	}
	return "pg_hint_plan hints do not reach application sessions (" + a.Readiness() +
		"); query hints stay advisory"
}

// A loaded module does not imply its product integration is usable. Lakebase
// exposes logs/plans through a separate opt-in history reader. Direct log-file
// auto_explain access and HypoPG remain unavailable.
func applyProviderCapabilityLimits(caps *ProviderCapabilities) {
	if normalizeProviderName(caps.Provider) == "lakebase" {
		caps.Extensions["auto_explain"] = "unavailable"
		caps.Extensions["hypopg"] = "unavailable"
	}
}

func collectExtensionEvidence(ctx context.Context, pool *pgxpool.Pool, caps *ProviderCapabilities) {
	rows, err := pool.Query(ctx, "SELECT extname FROM pg_catalog.pg_extension")
	if err != nil {
		caps.Limitations = append(caps.Limitations,
			"extension catalog probe failed; check connectivity and permissions")
		return
	}
	defer rows.Close()
	installed := make(map[string]bool)
	for rows.Next() {
		var name string
		if rows.Scan(&name) != nil {
			caps.Limitations = append(caps.Limitations, "extension catalog response could not be read")
			return
		}
		installed[name] = true
	}
	if rows.Err() != nil {
		caps.Limitations = append(caps.Limitations,
			"extension catalog read interrupted; refresh capabilities")
		return
	}
	for name := range caps.Extensions {
		if name != "auto_explain" {
			caps.Extensions[name] = "not_installed"
		}
	}
	for name := range installed {
		caps.Extensions[name] = "available"
	}
	collectModuleEvidence(ctx, pool, caps, installed["pg_hint_plan"])
}

func collectStatsPrivilege(ctx context.Context, pool *pgxpool.Pool, caps *ProviderCapabilities) {
	var allStats bool
	err := pool.QueryRow(ctx, `SELECT pg_has_role(current_user, 'pg_read_all_stats', 'USAGE')
		OR (SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname=current_user)`).Scan(&allStats)
	if err != nil {
		caps.Limitations = append(caps.Limitations,
			"statistics privilege probe failed; check role access")
		return
	}
	status := CapabilityStatus{Status: "limited", Reason: "only role-visible query statistics"}
	if allStats {
		status = CapabilityStatus{Status: "ok", Reason: "role can read all query statistics"}
	}
	caps.Permissions["read_stats"] = status
}

func collectModuleEvidence(
	ctx context.Context, pool *pgxpool.Pool, caps *ProviderCapabilities, hintsInstalled bool,
) {
	var autoExplain, hints *string
	err := pool.QueryRow(ctx, `SELECT
		(SELECT setting FROM pg_settings WHERE name='auto_explain.log_min_duration'),
		(SELECT setting FROM pg_settings WHERE name='pg_hint_plan.enable_hint')`).
		Scan(&autoExplain, &hints)
	if err != nil {
		caps.Extensions["pg_hint_plan"] = "unknown"
		caps.Limitations = append(caps.Limitations,
			"session module probe failed; check pg_settings access")
		return
	}
	caps.Extensions["auto_explain"] = "not_loaded"
	if autoExplain != nil {
		caps.Extensions["auto_explain"] = "available"
	}
	if hintsInstalled || hints != nil {
		caps.Extensions["pg_hint_plan"] = hintModuleState(hints)
	}
}

func hintModuleState(setting *string) string {
	if setting == nil {
		return "installed_not_loaded"
	}
	if *setting != "on" {
		return "installed_disabled"
	}
	return "available"
}
