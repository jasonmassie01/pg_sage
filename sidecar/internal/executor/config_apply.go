package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/pgconf"
)

// managedProviders are platforms where ALTER SYSTEM is disallowed; config
// changes must go through the provider's parameter group / database flags
// (and the provider reloads automatically), not ALTER SYSTEM.
var managedProviders = map[string]bool{
	"rds": true, "aurora": true, "aws": true,
	"cloud-sql": true, "cloudsql": true, "gcp": true,
	"alloydb": true, "neon": true, "supabase": true,
	"azure": true, "azure-flexible": true, "azure-single": true,
	"azure-cosmos": true,
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

// configParamFromSQL extracts the GUC name from an ALTER SYSTEM SET/RESET
// statement ("" if it is not one).
func configParamFromSQL(sql string) string {
	stmt, _ := pgconf.ParseAlterSystem(sql)
	return stmt.Name
}

// configApplyOutcome describes whether a config change is actually in
// effect after the ALTER SYSTEM ran, as read back from pg_settings.
type configApplyOutcome struct {
	InEffect       bool   // the read-back shows the requested value live
	PendingRestart bool   // written, but only a restart applies it
	State          readbackState
	Effective      string // pg_settings.setting after the reload
	Source         string // pg_settings.source after the reload
	Note           string // human-readable status for the action log
}

// applyConfigChange finalizes an ALTER SYSTEM config change so its effect
// (or lack of it) is accurate: it reloads the configuration and reads the
// setting back. Managed providers are flagged because the change should
// have gone through their parameter group instead.
func applyConfigChange(
	ctx context.Context,
	pool *pgxpool.Pool,
	sql, cloudEnv string,
	logFn func(string, string, ...any),
) configApplyOutcome {
	return applyConfigChangeWithin(ctx, pool, sql, cloudEnv, logFn, reloadSettleTimeout)
}

func applyConfigChangeWithin(
	ctx context.Context, pool *pgxpool.Pool, sql, cloudEnv string,
	logFn func(string, string, ...any), wait time.Duration,
) configApplyOutcome {
	stmt, ok := pgconf.ParseAlterSystem(sql)
	param := stmt.Name
	if isManagedProvider(cloudEnv) {
		return configApplyOutcome{State: readbackUnconfirmed,
			Note: managedConfigGuidance(cloudEnv, param)}
	}
	if !ok {
		return configApplyOutcome{State: readbackUnconfirmed,
			Note: "cannot parse the ALTER SYSTEM statement to read it back"}
	}
	if _, err := pool.Exec(ctx, "/* pg_sage */ SELECT pg_reload_conf()"); err != nil {
		if logFn != nil {
			logFn("executor", "pg_reload_conf after %s failed: %v", param, err)
		}
		return configApplyOutcome{State: readbackUnconfirmed,
			Note: param + " set; pg_reload_conf failed: " + err.Error()}
	}
	row, state, err := awaitReadback(ctx, pool, stmt, wait)
	if err != nil {
		return configApplyOutcome{State: readbackUnconfirmed,
			Note: param + " set and reloaded; read-back failed: " + err.Error()}
	}
	return describeReadback(param, row, state)
}

func describeReadback(param string, row settingRow, state readbackState) configApplyOutcome {
	out := configApplyOutcome{State: state, Effective: row.Setting, Source: row.Source,
		InEffect: state == readbackInEffect, PendingRestart: state == readbackPendingRestart}
	effective := fmt.Sprintf("%s%s (source: %s)", row.Setting, row.Unit, row.Source)
	switch state {
	case readbackInEffect:
		out.Note = param + " applied and reloaded; in effect at " + effective
	case readbackPendingRestart:
		out.Note = param + " written to postgresql.auto.conf; requires a PostgreSQL " +
			"restart to take effect (running value " + effective + ")"
	case readbackUnconfirmed:
		out.Note = param + " reloaded; effective value " + effective +
			" could not be attributed (sourcefile not visible to this role)"
	default:
		out.Note = param + " did not take effect after reload; effective value " + effective
	}
	return out
}
