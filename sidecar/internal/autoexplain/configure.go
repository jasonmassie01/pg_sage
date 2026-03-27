package autoexplain

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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

// ConfigureSession sets auto_explain parameters on a single pooled
// connection. If the availability method is session_load, it LOADs
// the extension first. Each SET is executed sequentially.
func ConfigureSession(
	ctx context.Context,
	conn *pgxpool.Conn,
	avail *Availability,
	scfg SessionConfig,
) error {
	if avail.Method == "session_load" {
		if _, err := conn.Exec(ctx, "LOAD 'auto_explain'"); err != nil {
			return fmt.Errorf("load auto_explain: %w", err)
		}
	}
	for _, stmt := range buildSetStatements(scfg) {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("configure session %q: %w", stmt, err)
		}
	}
	return nil
}

// ConfigureSessionBatch sets auto_explain parameters using a pgx
// Batch so all SETs travel in one network round-trip.
func ConfigureSessionBatch(
	ctx context.Context,
	conn *pgxpool.Conn,
	avail *Availability,
	scfg SessionConfig,
) error {
	batch := &pgx.Batch{}
	if avail.Method == "session_load" {
		batch.Queue("LOAD 'auto_explain'")
	}
	for _, stmt := range buildSetStatements(scfg) {
		batch.Queue(stmt)
	}
	br := conn.SendBatch(ctx, batch)
	defer br.Close()

	for range batch.Len() {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("configure session batch: %w", err)
		}
	}
	return nil
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
