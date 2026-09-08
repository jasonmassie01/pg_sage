package fleet

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func collectRuntimeEvidence(ctx context.Context, pool *pgxpool.Pool, caps *ProviderCapabilities) {
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
	collectStatsPrivilege(ctx, pool, caps)
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
