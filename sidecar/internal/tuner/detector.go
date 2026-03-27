package tuner

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DetectHintPlan checks if pg_hint_plan is available.
// Returns a non-nil HintPlanAvailability even when the
// extension is absent; that is not an error.
func DetectHintPlan(
	ctx context.Context,
	pool *pgxpool.Pool,
) (*HintPlanAvailability, error) {
	found, err := checkHintSharedPreload(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf(
			"detect pg_hint_plan: %w", err,
		)
	}
	if found {
		return buildAvailability(
			ctx, pool, true, false,
			"shared_preload",
		)
	}

	session, err := checkHintSessionLoad(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf(
			"detect pg_hint_plan: %w", err,
		)
	}
	if session {
		return buildAvailability(
			ctx, pool, false, true,
			"session_load",
		)
	}

	return &HintPlanAvailability{
		Available: false,
		Method:    "unavailable",
	}, nil
}

// buildAvailability fills out the struct and probes the
// hint_plan.hints table to see if hints can be persisted.
func buildAvailability(
	ctx context.Context,
	pool *pgxpool.Pool,
	shared, session bool,
	method string,
) (*HintPlanAvailability, error) {
	tableReady := checkHintTable(ctx, pool)
	return &HintPlanAvailability{
		SharedPreload:  shared,
		SessionLoad:    session,
		HintTableReady: tableReady,
		Available:      true,
		Method:         method,
	}, nil
}

// checkHintSharedPreload queries shared_preload_libraries
// for pg_hint_plan.
func checkHintSharedPreload(
	ctx context.Context,
	pool *pgxpool.Pool,
) (bool, error) {
	var libs string
	err := pool.QueryRow(
		ctx, "SHOW shared_preload_libraries",
	).Scan(&libs)
	if err != nil {
		return false, fmt.Errorf(
			"show shared_preload_libraries: %w", err,
		)
	}
	for _, lib := range strings.Split(libs, ",") {
		if strings.TrimSpace(lib) == "pg_hint_plan" {
			return true, nil
		}
	}
	return false, nil
}

// checkHintSessionLoad attempts LOAD 'pg_hint_plan' on a
// single connection. Permission errors are not propagated.
func checkHintSessionLoad(
	ctx context.Context,
	pool *pgxpool.Pool,
) (bool, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf(
			"acquire connection: %w", err,
		)
	}
	defer conn.Release()

	_, err = conn.Exec(ctx, "LOAD 'pg_hint_plan'")
	if err != nil {
		return false, nil
	}
	return true, nil
}

// checkHintTable probes hint_plan.hints to see if the table
// exists and is accessible.
func checkHintTable(
	ctx context.Context,
	pool *pgxpool.Pool,
) bool {
	_, err := pool.Exec(
		ctx,
		"SELECT 1 FROM hint_plan.hints LIMIT 0",
	)
	return err == nil
}
