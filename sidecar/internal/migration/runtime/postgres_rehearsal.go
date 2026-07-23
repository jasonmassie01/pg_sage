package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/executor"
	planpkg "github.com/pg-sage/sidecar/internal/migration/plan"
	rehearsalpkg "github.com/pg-sage/sidecar/internal/migration/rehearsal"
)

type PostgresRehearsalRunner struct {
	lockTimeout      time.Duration
	statementTimeout time.Duration
}

func NewPostgresRehearsalRunner(
	lockTimeout, statementTimeout time.Duration,
) *PostgresRehearsalRunner {
	return &PostgresRehearsalRunner{
		lockTimeout: lockTimeout, statementTimeout: statementTimeout,
	}
}

func (runner *PostgresRehearsalRunner) Run(
	ctx context.Context, dsn string, planned planpkg.Plan,
) (rehearsalpkg.Measurement, error) {
	if runner == nil || dsn == "" {
		return rehearsalpkg.Measurement{}, fmt.Errorf("rehearsal clone DSN is unavailable")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return rehearsalpkg.Measurement{}, fmt.Errorf("connect rehearsal clone: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return rehearsalpkg.Measurement{}, fmt.Errorf("ping rehearsal clone: %w", err)
	}
	before, err := rehearsalDatabaseSize(ctx, pool)
	if err != nil {
		return rehearsalpkg.Measurement{}, err
	}
	measurement := rehearsalpkg.Measurement{}
	for _, step := range planned.ExpandSteps {
		started := time.Now()
		if err := runner.applyStep(ctx, pool, step); err != nil {
			return rehearsalpkg.Measurement{}, err
		}
		duration := time.Since(started)
		measurement.BackfillDuration += duration
		if duration > measurement.MaxLockDuration {
			measurement.MaxLockDuration = duration
		}
	}
	after, err := rehearsalDatabaseSize(ctx, pool)
	if err != nil {
		return rehearsalpkg.Measurement{}, err
	}
	measurement.DiskDeltaBytes = after - before
	return measurement, nil
}

func (runner *PostgresRehearsalRunner) applyStep(
	ctx context.Context, pool *pgxpool.Pool, step planpkg.Step,
) error {
	if err := executor.ValidateExecutorSQL(step.SQL); err != nil {
		return fmt.Errorf("validate rehearsal step %s: %w", step.Kind, err)
	}
	lockMS := int(runner.lockTimeout / time.Millisecond)
	var err error
	if step.RequiresTopLevel {
		err = executor.ExecConcurrently(
			ctx, pool, step.SQL, runner.statementTimeout,
			executor.WithLockTimeout(lockMS),
		)
	} else {
		err = executor.ExecInTransaction(
			ctx, pool, step.SQL, runner.statementTimeout,
			executor.WithLockTimeout(lockMS),
		)
	}
	if err != nil {
		return fmt.Errorf("rehearse migration step %s: %w", step.Kind, err)
	}
	return nil
}

func rehearsalDatabaseSize(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var size int64
	if err := pool.QueryRow(ctx,
		`SELECT pg_database_size(current_database())`).Scan(&size); err != nil {
		return 0, fmt.Errorf("measure rehearsal clone size: %w", err)
	}
	return size, nil
}

var _ rehearsalpkg.Runner = (*PostgresRehearsalRunner)(nil)
