package autoexplain

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// SessionConfig holds auto_explain session parameters.
type SessionConfig struct {
	LogMinDurationMs int  // minimum query duration to log plans
	LogAnalyze       bool // include ANALYZE timing
	LogBuffers       bool // include buffer usage
	LogNested        bool // include nested statements
}

// DefaultSessionConfig returns a SessionConfig with sensible
// defaults for the given slow-query threshold.
func DefaultSessionConfig(slowQueryThresholdMs int) SessionConfig {
	return SessionConfig{
		LogMinDurationMs: slowQueryThresholdMs,
		LogAnalyze:       true,
		LogBuffers:       true,
		LogNested:        true,
	}
}

// ConfigureTransaction applies auto_explain parameters with SET LOCAL
// inside tx, so they end with the transaction and never leak into the
// pooled session used by other sidecar work (G1-B15). With the
// session_load method it LOADs the module first (loading alone changes no
// behavior: every auto_explain GUC keeps its default). A permission error
// on one SET is tolerated (e.g. managed services that pin the value via
// ALTER ROLE); a savepoint keeps the transaction usable afterwards.
func ConfigureTransaction(
	ctx context.Context,
	tx pgx.Tx,
	avail *Availability,
	scfg SessionConfig,
) error {
	if avail.Method == "session_load" {
		if _, err := tx.Exec(ctx, "LOAD 'auto_explain'"); err != nil {
			return fmt.Errorf("load auto_explain: %w", err)
		}
	}
	for _, stmt := range buildSetStatements(scfg) {
		local := "SET LOCAL " + strings.TrimPrefix(stmt, "SET ")
		if err := execTolerantSet(ctx, tx, local); err != nil {
			return err
		}
	}
	return nil
}

func execTolerantSet(ctx context.Context, tx pgx.Tx, stmt string) error {
	if _, err := tx.Exec(ctx, "SAVEPOINT pg_sage_autoexplain_set"); err != nil {
		return fmt.Errorf("savepoint before %q: %w", stmt, err)
	}
	if _, err := tx.Exec(ctx, stmt); err != nil {
		if !isPermissionDenied(err) {
			return fmt.Errorf("configure %q: %w", stmt, err)
		}
		if _, rbErr := tx.Exec(ctx,
			"ROLLBACK TO SAVEPOINT pg_sage_autoexplain_set"); rbErr != nil {
			return fmt.Errorf("rollback savepoint after %q: %w", stmt, rbErr)
		}
	}
	return nil
}

// isPermissionDenied checks whether the error is a PG permission
// denied error (SQLSTATE 42501).
func isPermissionDenied(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42501" {
		return true
	}
	// Fallback: check error string for SQLSTATE 42501.
	return strings.Contains(err.Error(), "42501")
}

// buildSetStatements returns the SET statements for the given
// session config.
func buildSetStatements(scfg SessionConfig) []string {
	stmts := []string{
		fmt.Sprintf(
			"SET auto_explain.log_min_duration = '%dms'",
			scfg.LogMinDurationMs,
		),
		"SET auto_explain.log_format = 'json'",
	}
	if scfg.LogAnalyze {
		stmts = append(
			stmts,
			"SET auto_explain.log_analyze = true",
		)
	}
	if scfg.LogBuffers {
		stmts = append(
			stmts,
			"SET auto_explain.log_buffers = true",
		)
	}
	if scfg.LogNested {
		stmts = append(
			stmts,
			"SET auto_explain.log_nested_statements = true",
		)
	}
	return stmts
}
