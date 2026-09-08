package autoexplain

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Availability describes how auto_explain can be used on this
// PostgreSQL instance.
type Availability struct {
	SharedPreload bool   // loaded via shared_preload_libraries
	SessionLoad   bool   // can use LOAD 'auto_explain' per-session
	AlreadyLoaded bool   // module GUC is registered in an existing provider-managed session
	Available     bool   // at least one method works
	Method        string // "shared_preload", "already_loaded", "session_load", "unavailable"
}

// Detect probes the database to determine auto_explain availability.
// It returns a non-nil Availability even when auto_explain is absent;
// that is not an error — it is expected on many managed DB services.
func Detect(
	ctx context.Context,
	pool *pgxpool.Pool,
) (*Availability, error) {
	found, err := checkSharedPreload(ctx, pool)
	if err != nil && !isPermissionDenied(err) {
		return nil, fmt.Errorf("detect auto_explain: %w", err)
	}
	if found {
		return &Availability{
			SharedPreload: true,
			Available:     true,
			Method:        "shared_preload",
		}, nil
	}

	registered, err := checkRegisteredModule(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf("detect auto_explain settings: %w", err)
	}
	if registered {
		return &Availability{AlreadyLoaded: true, Available: true, Method: "already_loaded"}, nil
	}

	session, err := checkSessionLoad(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf("detect auto_explain: %w", err)
	}
	if session {
		return &Availability{
			SessionLoad: true,
			Available:   true,
			Method:      "session_load",
		}, nil
	}

	return &Availability{
		Available: false,
		Method:    "unavailable",
	}, nil
}

// pg_settings contains registered module GUCs; current_setting alone accepts placeholder GUCs.
func checkRegisteredModule(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var found bool
	err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_settings
		WHERE name='auto_explain.log_min_duration' AND vartype='integer')`).Scan(&found)
	return found, err
}

// checkSharedPreload queries shared_preload_libraries for
// auto_explain.
func checkSharedPreload(
	ctx context.Context,
	pool *pgxpool.Pool,
) (bool, error) {
	var libs string
	err := pool.QueryRow(
		ctx,
		"SHOW shared_preload_libraries",
	).Scan(&libs)
	if err != nil {
		return false, fmt.Errorf(
			"show shared_preload_libraries: %w", err,
		)
	}
	for _, lib := range strings.Split(libs, ",") {
		if strings.TrimSpace(lib) == "auto_explain" {
			return true, nil
		}
	}
	return false, nil
}

// checkSessionLoad attempts LOAD 'auto_explain' on a single
// connection. Returns true if the extension can be loaded
// per-session, false otherwise. Permission errors are not
// propagated — they mean session loading is unavailable.
func checkSessionLoad(
	ctx context.Context,
	pool *pgxpool.Pool,
) (bool, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	_, err = conn.Exec(ctx, "LOAD 'auto_explain'")
	if err != nil {
		// Permission denied or extension not available — expected.
		return false, nil
	}
	return true, nil
}

// EnableHint returns a platform-specific instruction for enabling
// auto_explain. Returns an empty string for unknown / self-managed
// platforms (where the user already controls postgresql.conf).
func EnableHint(platform string) string {
	switch platform {
	case "cloud-sql":
		return "set the `cloudsql.enable_auto_explain` flag to `on` " +
			"(adds auto_explain to shared_preload_libraries; requires restart): " +
			"`gcloud sql instances patch INSTANCE " +
			"--database-flags=cloudsql.enable_auto_explain=on`"
	case "alloydb":
		return "add `auto_explain` to the `google_db_advisor.shared_preload_libraries` " +
			"flag on the AlloyDB cluster (requires restart)"
	case "rds", "aurora":
		return "add `auto_explain` to `shared_preload_libraries` in the RDS/Aurora " +
			"DB parameter group (requires reboot)"
	case "azure":
		return "add `AUTO_EXPLAIN` to the `azure.extensions` server parameter " +
			"on the Azure Database for PostgreSQL flexible server (requires restart)"
	}
	return ""
}
