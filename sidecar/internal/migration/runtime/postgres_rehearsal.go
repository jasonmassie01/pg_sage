package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/executor"
	planpkg "github.com/pg-sage/sidecar/internal/migration/plan"
	rehearsalpkg "github.com/pg-sage/sidecar/internal/migration/rehearsal"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

type PostgresRehearsalRunner struct {
	lockTimeout      time.Duration
	statementTimeout time.Duration
	// production identifies the server the clone must not be; without it
	// Run refuses (migration-07).
	production IdentitySource
	// workload reads the statements to measure before and after the
	// expand steps; analyzeTimeout bounds each executed one on the clone
	// and inspect is the analyze guard's parser (nil: libpg_query).
	workload       WorkloadSource
	analyzeTimeout time.Duration
	inspect        func(string) (sqlast.ReadQuery, error)
}

func NewPostgresRehearsalRunner(
	lockTimeout, statementTimeout time.Duration,
) *PostgresRehearsalRunner {
	return &PostgresRehearsalRunner{
		lockTimeout: lockTimeout, statementTimeout: statementTimeout,
	}
}

// WithProduction sets the production server a clone is checked against
// before the first step.
func (runner *PostgresRehearsalRunner) WithProduction(
	source IdentitySource,
) *PostgresRehearsalRunner {
	runner.production = source
	return runner
}

// WithWorkload measures source's statements on the clone before and after
// the expand steps (lab-04b), each executed one bounded by timeout (0:
// clone.rehearsal_statement_timeout_seconds' default).
func (runner *PostgresRehearsalRunner) WithWorkload(
	source WorkloadSource, timeout time.Duration,
) *PostgresRehearsalRunner {
	runner.workload, runner.analyzeTimeout = source, timeout
	return runner
}

func (runner *PostgresRehearsalRunner) Run(
	ctx context.Context, dsn string, planned planpkg.Plan,
) (rehearsalpkg.Measurement, error) {
	if runner == nil || dsn == "" {
		return rehearsalpkg.Measurement{}, fmt.Errorf("rehearsal clone DSN is unavailable")
	}
	pool, err := runner.connectClone(ctx, dsn)
	if err != nil {
		return rehearsalpkg.Measurement{}, err
	}
	defer pool.Close()
	stmts, note := runner.affected(ctx, planned.Table)
	queriesBefore, err := runner.measureSide(ctx, pool, stmts, nil)
	if err != nil {
		return rehearsalpkg.Measurement{}, err
	}
	measurement, err := runner.applySteps(ctx, pool, planned)
	if err != nil {
		return rehearsalpkg.Measurement{}, err
	}
	queriesAfter, err := runner.measureSide(ctx, pool, stmts, queriesBefore)
	if err != nil {
		return rehearsalpkg.Measurement{}, err
	}
	measurement.AffectedQueries = pairAll(stmts, queriesBefore, queriesAfter)
	measurement.WorkloadNote = note
	return measurement, nil
}

// applySteps runs the expand steps on the clone, timing each, and the
// database's growth over them.
func (runner *PostgresRehearsalRunner) applySteps(
	ctx context.Context, pool *pgxpool.Pool, planned planpkg.Plan,
) (rehearsalpkg.Measurement, error) {
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
		measurement.MaxStepDuration = max(measurement.MaxStepDuration, duration)
		measurement.Steps = append(measurement.Steps, rehearsalpkg.StepMeasurement{
			Kind: step.Kind, Duration: duration, Concurrent: step.RequiresTopLevel,
			LockLevel: planpkg.StepLock(ctx, step),
		})
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
