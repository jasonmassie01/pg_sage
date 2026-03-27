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
	Available     bool   // at least one method works
	Method        string // "shared_preload", "session_load", "unavailable"
}

// Detect probes the database to determine auto_explain availability.
// It returns a non-nil Availability even when auto_explain is absent;
// that is not an error — it is expected on many managed DB services.
func Detect(
	ctx context.Context,
	pool *pgxpool.Pool,
) (*Availability, error) {
	found, err := checkSharedPreload(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf("detect auto_explain: %w", err)
	}
	if found {
		return &Availability{
			SharedPreload: true,
			Available:     true,
			Method:        "shared_preload",
		}, nil
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
