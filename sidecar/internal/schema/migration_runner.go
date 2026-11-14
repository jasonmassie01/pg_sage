package schema

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Migration step budget (config-04). Every step has its own time budget and
// waits at most migrationLockTimeout for a table lock: a step queued behind
// a long lock (a nightly pg_dump holds ACCESS SHARE on every table) would
// otherwise queue every application session on that table behind it. A
// step that cannot get its lock is retried after a jittered backoff while
// its budget lasts, then fails the boot naming the step.
const (
	migrationStepTimeout = 10 * time.Second
	migrationBootCap     = 5 * time.Minute
	migrationLockTimeout = 2 * time.Second
	sqlLockNotAvailable  = "55P03"
)

// stepLockTimeoutSQL bounds a step's lock waits when it may run on a
// session other than the bootstrap's (which sets lock_timeout itself); it
// is migrationLockTimeout, which the retry budget assumes (config-04b).
var stepLockTimeoutSQL = fmt.Sprintf("SET LOCAL lock_timeout = '%dms'",
	migrationLockTimeout.Milliseconds())

// migrationLockBackoff is the wait before each retry of a step that hit
// lock_timeout; each wait is jittered by +-25%.
var migrationLockBackoff = []time.Duration{
	500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second,
}

// migrationReport sums up one boot's migration steps for the INFO line.
type migrationReport struct {
	steps, applied, rerun, skipped int
	recorded                       int // skipped by the ledger (config-04b)
	elapsed                        time.Duration
	slowest                        string
	slowestTook                    time.Duration
}

func (r migrationReport) String() string {
	s := fmt.Sprintf("schema: %d steps, %d applied, %d rerun, %d skipped, %d ms",
		r.steps, r.applied, r.rerun, r.skipped, r.elapsed.Milliseconds())
	if r.recorded > 0 {
		s += fmt.Sprintf("; %d recorded", r.recorded)
	}
	if r.slowest != "" {
		s += fmt.Sprintf("; slowest %s %d ms", r.slowest, r.slowestTook.Milliseconds())
	}
	return s
}

// count adds one step's outcome. Applied and rerun steps compete for
// slowest; applied steps are logged at DEBUG.
func (r *migrationReport) count(name string, outcome stepOutcome, took time.Duration) {
	switch outcome {
	case stepLedgered:
		r.recorded++
		return
	case stepSkipped:
		r.skipped++
		return
	case stepRerun:
		r.rerun++
	case stepApplied:
		r.applied++
		slog.Debug("schema: migration step applied", "step", name, "ms", took.Milliseconds())
	}
	if r.slowest == "" || took > r.slowestTook {
		r.slowest, r.slowestTook = name, took
	}
}

// migrationBudget is the whole list's budget: the sum of the step budgets,
// capped at migrationBootCap.
func migrationBudget(steps int) time.Duration {
	return min(time.Duration(steps)*migrationStepTimeout, migrationBootCap)
}

// runMigrations applies idempotent schema changes to existing installs.
func runMigrations(ctx context.Context, db bootstrapDB) error {
	_, err := runMigrationSteps(ctx, db, migrationSteps())
	return err
}

// runMigrationSteps runs steps in order and logs one INFO summary line,
// also when a step fails or the list is empty.
func runMigrationSteps(
	ctx context.Context, db bootstrapDB, steps []migrationStep,
) (migrationReport, error) {
	report := migrationReport{steps: len(steps)}
	start := time.Now()
	defer func() {
		report.elapsed = time.Since(start)
		slog.Info(report.String())
	}()
	if len(steps) == 0 {
		return report, nil
	}
	bctx, cancel := withBudget(ctx, listBudget(len(steps)))
	defer cancel()
	for _, step := range steps {
		stepStart := time.Now()
		outcome, err := runMigrationStep(bctx, db, step)
		took := time.Since(stepStart)
		if err != nil {
			return report, err
		}
		report.count(step.name, outcome, took)
	}
	return report, nil
}

// stepOutcome is what one migration step did.
type stepOutcome int

const (
	// stepSkipped: the catalog showed every statement done; nothing ran.
	stepSkipped stepOutcome = iota
	// stepRerun: only every-boot statements ran (DO blocks, functions, DML).
	stepRerun
	// stepApplied: a statement the catalog lacked, or of unknown shape, ran.
	stepApplied
	// stepLedgered: sage.schema_migrations records the step; nothing ran.
	stepLedgered
)

// runMigrationStep runs step unless the bootstrap's migration ledger
// records it and trusts the catalog (config-04b), then records it.
// Outside a bootstrap (no ledger in ctx) it is runCheckedStep. A timeout
// names the deadline that ended it (ci-15).
func runMigrationStep(
	ctx context.Context, db bootstrapDB, step migrationStep,
) (stepOutcome, error) {
	outcome, err := runLedgeredStep(ctx, db, step)
	return outcome, namedDeadline(ctx, err)
}

func runLedgeredStep(
	ctx context.Context, db bootstrapDB, step migrationStep,
) (stepOutcome, error) {
	l := ledgerFrom(ctx)
	if err := l.load(ctx, db); err != nil {
		return stepSkipped, err
	}
	skip, err := l.decide(step)
	if err != nil {
		return stepSkipped, err
	}
	if skip {
		return runDataStatements(ctx, db, step)
	}
	if outcome, ran, err := runDeferredVariant(ctx, db, step); ran || err != nil {
		return outcome, err // config-04c: large indexes left to the builder
	}
	outcome, err := runCheckedStep(ctx, db, step)
	if err != nil {
		return outcome, err
	}
	return outcome, l.record(ctx, db, step)
}

// runDataStatements runs only the data statements of a step the ledger
// skips (config-04b); a step without one runs nothing.
func runDataStatements(
	ctx context.Context, db bootstrapDB, step migrationStep,
) (stepOutcome, error) {
	data := dataStatements(step)
	if data == "" {
		return stepLedgered, nil
	}
	return runCheckedStep(ctx, db, migrationStep{name: step.name, sql: data})
}

// runCheckedStep runs the statements of step the catalog does not show
// done, in one transaction, retrying lock_timeout failures.
func runCheckedStep(
	ctx context.Context, db bootstrapDB, step migrationStep,
) (stepOutcome, error) {
	sctx, cancel := context.WithTimeout(ctx, migrationStepTimeout)
	defer cancel()
	stmts := classifyStep(step.sql)
	if err := markDone(sctx, db, stmts); err != nil {
		return stepSkipped, fmt.Errorf("migration step %s: precheck: %w", step.name, err)
	}
	if !pendingWork(stmts) {
		return stepSkipped, nil
	}
	for attempt := 0; ; attempt++ {
		outcome, err := applyStepTx(sctx, db, stmts)
		if err == nil {
			return outcome, nil
		}
		if !isLockNotAvailable(err) {
			return stepSkipped, fmt.Errorf("migration step %s: %w", step.name, err)
		}
		if !waitLockRetry(sctx, attempt) {
			return stepSkipped, fmt.Errorf("migration step %s: table lock not granted in "+
				"%d attempts (lock_timeout %s each, step budget %s); another session "+
				"holds a conflicting lock, the next start retries: %w", step.name,
				attempt+1, migrationLockTimeout, migrationStepTimeout, err)
		}
	}
}

// applyStepTx runs applyStep in its own transaction and commits it.
func applyStepTx(
	ctx context.Context, db bootstrapDB, stmts []migrationStmt,
) (stepOutcome, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return stepSkipped, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	outcome, err := applyStep(ctx, tx, stmts)
	if err != nil {
		return stepSkipped, err
	}
	if err := tx.Commit(ctx); err != nil {
		return stepSkipped, fmt.Errorf("commit: %w", err)
	}
	return outcome, nil
}

// applyStep re-reads the facts of stmts on tx, then runs the statements not
// done, in order, batched into as few round trips as it can: after an
// opaque statement (DO, DML, unknown) the facts of the rest are read again.
func applyStep(ctx context.Context, tx bootstrapDB, stmts []migrationStmt) (stepOutcome, error) {
	if err := markDone(ctx, tx, stmts); err != nil {
		return stepSkipped, err
	}
	outcome := stepSkipped
	var batch []string
	work := false
	for i, s := range stmts {
		if s.done {
			continue
		}
		if s.shape == shapeIndex {
			batch = append(batch, dropInvalidIndexSQL(s.checks[0].rel))
		}
		batch = append(batch, s.sql)
		if s.shape == shapeSetting {
			continue
		}
		work = true
		outcome = max(outcome, statementOutcome(s.shape))
		if s.shape == shapeEveryBoot || s.shape == shapeUnknown {
			if rest := stmts[i+1:]; hasChecks(rest) {
				if _, err := tx.Exec(ctx, strings.Join(batch, ";")); err != nil {
					return stepSkipped, err
				}
				batch, work = nil, false
				if err := markDone(ctx, tx, rest); err != nil {
					return stepSkipped, err
				}
			}
		}
	}
	if work {
		if _, err := tx.Exec(ctx, strings.Join(batch, ";")); err != nil {
			return stepSkipped, err
		}
	}
	return outcome, nil
}

// dropInvalidIndexSQL drops index rel (sage.name, a plain identifier the
// classifier matched) when it is INVALID (config-04b): a killed CREATE
// INDEX CONCURRENTLY leaves one, and CREATE INDEX IF NOT EXISTS keeps it.
func dropInvalidIndexSQL(rel string) string {
	return fmt.Sprintf(`DO $invalid_index$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_index
                WHERE indexrelid = pg_catalog.to_regclass('%[1]s') AND NOT indisvalid) THEN
        DROP INDEX %[1]s;
    END IF;
END $invalid_index$`, rel)
}

func statementOutcome(shape stmtShape) stepOutcome {
	if shape == shapeEveryBoot {
		return stepRerun
	}
	return stepApplied
}

// waitLockRetry sleeps before retry attempt+1 and reports whether to retry:
// not after the last backoff, nor when the step budget would end before
// the retry could wait out its lock_timeout.
func waitLockRetry(ctx context.Context, attempt int) bool {
	if attempt >= len(migrationLockBackoff) {
		return false
	}
	base := migrationLockBackoff[attempt]
	wait := base*3/4 + rand.N(base/2+1)
	if deadline, ok := ctx.Deadline(); ok &&
		time.Until(deadline) < wait+migrationLockTimeout {
		return false
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func isLockNotAvailable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == sqlLockNotAvailable
}
