package executor

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// restartRequiredParams are GUCs whose change only takes effect after a
// PostgreSQL restart — pg_sage cannot restart the server, so an
// ALTER SYSTEM for these is written to postgresql.auto.conf but does not
// apply until the operator restarts.
var restartRequiredParams = map[string]bool{
	"shared_buffers":           true,
	"max_connections":          true,
	"wal_buffers":              true,
	"max_worker_processes":     true,
	"max_prepared_transactions": true,
	"max_wal_senders":          true,
	"max_replication_slots":    true,
	"huge_pages":               true,
	"shared_preload_libraries": true,
	"max_locks_per_transaction": true,
	"superuser_reserved_connections": true,
}

// managedProviders are platforms where ALTER SYSTEM is disallowed; config
// changes must go through the provider's parameter group / database flags
// (and the provider reloads automatically), not ALTER SYSTEM.
var managedProviders = map[string]bool{
	"rds": true, "aurora": true, "aws": true,
	"cloud-sql": true, "cloudsql": true, "gcp": true,
	"alloydb": true,
	"azure": true, "azure-flexible": true, "azure-single": true,
}

// outcomeStatus maps whether a config change is live to an action_log
// outcome: "success" when in effect, "applied_pending_restart" when the
// value was written but a restart is still required to apply it.
func outcomeStatus(inEffect bool) string {
	if inEffect {
		return "success"
	}
	return "applied_pending_restart"
}

// isManagedProvider reports whether the cloud environment is a managed
// PostgreSQL service where ALTER SYSTEM is blocked.
func isManagedProvider(env string) bool {
	return managedProviders[strings.ToLower(strings.TrimSpace(env))]
}

// isAlterSystem reports whether sql is an ALTER SYSTEM SET/RESET statement.
func isAlterSystem(sql string) bool {
	return strings.HasPrefix(
		strings.ToUpper(strings.TrimSpace(sql)), "ALTER SYSTEM")
}

// configParamFromSQL extracts the first GUC name from an
// "ALTER SYSTEM SET/RESET <param> ..." statement. Returns "" if not found.
func configParamFromSQL(sql string) string {
	upper := strings.ToUpper(sql)
	var rest string
	switch {
	case strings.Contains(upper, "ALTER SYSTEM SET "):
		i := strings.Index(upper, "ALTER SYSTEM SET ")
		rest = sql[i+len("ALTER SYSTEM SET "):]
	case strings.Contains(upper, "ALTER SYSTEM RESET "):
		i := strings.Index(upper, "ALTER SYSTEM RESET ")
		rest = sql[i+len("ALTER SYSTEM RESET "):]
	default:
		return ""
	}
	rest = strings.TrimSpace(rest)
	// param ends at the first space, '=', or ';'
	end := strings.IndexAny(rest, " =;")
	if end >= 0 {
		rest = rest[:end]
	}
	return strings.Trim(strings.TrimSpace(rest), `"'`)
}

// configApplyOutcome describes how a config change should be applied and
// whether it is actually in effect after the ALTER SYSTEM ran.
type configApplyOutcome struct {
	InEffect bool   // true once the change is live
	Note     string // human-readable status for the action log
}

// applyConfigChange finalizes an ALTER SYSTEM config change so its effect
// (or lack of it) is accurate. On a self-managed server it reloads the
// configuration for reload-only GUCs; restart-only GUCs are written but
// flagged as needing a restart. Managed providers are flagged because the
// change should have gone through their parameter group instead.
func applyConfigChange(
	ctx context.Context,
	pool *pgxpool.Pool,
	sql, cloudEnv string,
	logFn func(string, string, ...any),
) configApplyOutcome {
	param := configParamFromSQL(sql)

	if isManagedProvider(cloudEnv) {
		return configApplyOutcome{
			InEffect: false,
			Note: "managed provider (" + cloudEnv + "): apply " + param +
				" via the provider parameter group / database flags — " +
				"ALTER SYSTEM does not take effect here",
		}
	}
	if restartRequiredParams[strings.ToLower(param)] {
		return configApplyOutcome{
			InEffect: false,
			Note: param + " written to postgresql.auto.conf; " +
				"requires a PostgreSQL restart to take effect",
		}
	}
	// Self-managed, reload-only parameter: reload so it takes effect now.
	if _, err := pool.Exec(ctx,
		"/* pg_sage */ SELECT pg_reload_conf()"); err != nil {
		if logFn != nil {
			logFn("executor", "pg_reload_conf after %s failed: %v", param, err)
		}
		return configApplyOutcome{
			InEffect: false,
			Note:     param + " set; pg_reload_conf failed: " + err.Error(),
		}
	}
	return configApplyOutcome{
		InEffect: true,
		Note:     param + " applied and reloaded (in effect)",
	}
}
