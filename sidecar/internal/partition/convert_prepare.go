package partition

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// chooseCut is the history partition's upper bound: the second UTC
// midnight after now, or after the newest row when rows are dated ahead.
// Rows written until the exclusive lock (at least a day) stay below it.
func chooseCut(ctx context.Context, s DB, t Table) (time.Time, error) {
	var now time.Time
	var newest *time.Time
	if err := s.QueryRow(ctx, fmt.Sprintf(`SELECT now(), (SELECT max(%s) FROM %s)`,
		ident(t.Column), t.ident())).Scan(&now, &newest); err != nil {
		return time.Time{}, fmt.Errorf("read the newest %s: %w", t.Column, err)
	}
	base := now
	if newest != nil && newest.After(now) {
		if newest.Sub(now) > maxAhead {
			return time.Time{}, fmt.Errorf("%w: the newest %s is %s (now %s)", ErrFutureRows,
				t.Column, newest.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
		}
		base = *newest
	}
	return DayStart(base).Add(2 * day), nil
}

// prepare adds and validates the CHECK proving the history bound and
// pre-builds the partitioned key, all without an exclusive lock held
// across a heap scan. It returns how long VALIDATE took.
func prepare(ctx context.Context, s DB, t Table, cut time.Time) (time.Duration, error) {
	if err := runHook(ctx, s, "prepare"); err != nil {
		return 0, err
	}
	// The CHECK also proves the key columns NOT NULL, so no SET NOT NULL
	// or ADD PRIMARY KEY in the exclusive transaction scans for NULLs.
	conds := make([]string, 0, len(t.Key)+2)
	for _, k := range append(append([]string{}, t.Key...), t.Column) {
		conds = append(conds, ident(k)+" IS NOT NULL")
	}
	conds = append(conds, ident(t.Column)+" < "+literal(cut))
	// NOT VALID: a catalog change (a brief ACCESS EXCLUSIVE, no scan).
	if _, err := s.Exec(ctx, fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s) NOT VALID",
		t.ident(), ident(t.checkName()), strings.Join(conds, " AND "))); err != nil {
		return 0, fmt.Errorf("add the cutover CHECK: %w", err)
	}
	if err := runHook(ctx, s, "validate"); err != nil {
		return 0, err
	}
	start := time.Now()
	if _, err := s.Exec(ctx, fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s", t.ident(),
		ident(t.checkName()))); err != nil {
		return 0, fmt.Errorf("validate the cutover CHECK: %w", err)
	}
	validated := time.Since(start)
	if len(t.Key) == 0 {
		return validated, nil
	}
	if err := runHook(ctx, s, "index"); err != nil {
		return validated, err
	}
	cols := make([]string, 0, len(t.Key)+1)
	for _, k := range append(append([]string{}, t.Key...), t.Column) {
		cols = append(cols, ident(k))
	}
	how, err := keyBuild(ctx, s, t)
	if err != nil {
		return validated, err
	}
	if _, err := s.Exec(ctx, fmt.Sprintf("CREATE UNIQUE INDEX %s%s ON %s (%s)", how,
		ident(t.keyName()), t.ident(), strings.Join(cols, ", "))); err != nil {
		return validated, fmt.Errorf("build the partitioned key: %w", err)
	}
	return validated, nil
}

// cutover is the ACCESS EXCLUSIVE transaction. It returns how long the
// lock was held.
func cutover(ctx context.Context, s DB, t Table, cut time.Time) (time.Duration, error) {
	tx, err := s.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockedSession(ctx, tx, "sage.partition."+t.Name, LockTimeout); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '"+
		ms(exclusiveStatementTimeout)+"'"); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, "LOCK TABLE "+t.ident()+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		return 0, fmt.Errorf("lock: %w", err)
	}
	start := time.Now()
	if err := requireValidCheck(ctx, tx, t); err != nil {
		return 0, err
	}
	plan, err := readPlan(ctx, tx, t, cut)
	if err != nil {
		return 0, err
	}
	before, attach := convertSteps(t, plan)
	if err := execSteps(ctx, tx, before); err != nil {
		return 0, err
	}
	if err := runHook(ctx, tx, "attach"); err != nil {
		return 0, err
	}
	if err := execSteps(ctx, tx, attach); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return time.Since(start), nil
}

func execSteps(ctx context.Context, db DB, steps []string) error {
	for _, step := range steps {
		if _, err := db.Exec(ctx, step); err != nil {
			return fmt.Errorf("%s: %w", firstLine(step), err)
		}
	}
	return nil
}

// requireValidCheck re-checks, under the exclusive lock, that the CHECK
// the ATTACH relies on is there and validated.
func requireValidCheck(ctx context.Context, db DB, t Table) error {
	var valid bool
	err := db.QueryRow(ctx, `SELECT COALESCE((SELECT convalidated FROM pg_catalog.pg_constraint
		WHERE conrelid = pg_catalog.to_regclass($1) AND conname = $2), false)`,
		t.regclass(), t.checkName()).Scan(&valid)
	if err != nil {
		return fmt.Errorf("read the cutover CHECK: %w", err)
	}
	if !valid {
		return fmt.Errorf("the cutover CHECK on %s is missing or not validated", t.regclass())
	}
	return nil
}

// Cleanup removes what an unfinished conversion of the plain table t left
// (the cutover CHECK, which refuses rows once the clock passes the cut, and
// the pre-built key). It does nothing while another session converts t.
func Cleanup(ctx context.Context, db DB, t Table) error {
	if p, ok := db.(acquirer); ok {
		conn, err := p.Acquire(ctx)
		if err != nil {
			return fmt.Errorf("partition: clean up %s: %w", t.regclass(), err)
		}
		defer conn.Release()
		db = conn
	}
	_, err := withConvertLock(ctx, db, t, func(s DB) (Result, error) {
		return Result{}, cleanupLocked(ctx, s, t)
	})
	if errors.Is(err, ErrBusy) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("partition: clean up %s: %w", t.regclass(), err)
	}
	return nil
}

// cleanupLocked drops the cutover CHECK and the pre-built key, if any, on
// session s holding the conversion lock.
func cleanupLocked(ctx context.Context, s DB, t Table) (err error) {
	var hasCheck, hasKey bool
	if err := s.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
		WHERE conrelid = pg_catalog.to_regclass($1) AND conname = $2),
		pg_catalog.to_regclass($3) IS NOT NULL`, t.regclass(), t.checkName(),
		"sage."+t.keyName()).Scan(&hasCheck, &hasKey); err != nil {
		return fmt.Errorf("look for leftovers: %w", err)
	}
	if !hasCheck && !hasKey {
		return nil
	}
	// A failed step may have left the session's statement timeout anywhere.
	restore, err := setSession(ctx, s, LockTimeout, cleanupTimeout)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, restore(ctx)) }()
	if hasCheck {
		if _, err := s.Exec(ctx, fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s",
			t.ident(), ident(t.checkName()))); err != nil {
			return fmt.Errorf("drop the cutover CHECK: %w", err)
		}
	}
	if hasKey {
		// Not CONCURRENTLY: that waits for every older snapshot in the database,
		// which deadlocks against a session waiting for a lock this one holds
		// (bootstrap). Dropping is a brief lock under the lock timeout.
		if _, err := s.Exec(ctx, "DROP INDEX IF EXISTS "+child(t.keyName())); err != nil {
			return fmt.Errorf("drop the pre-built key: %w", err)
		}
	}
	return nil
}

// Hint says what an operator can do about a failed conversion.
func Hint(err error) string {
	switch {
	case errors.Is(err, ErrFutureRows):
		return "Rows dated more than a week ahead (a skewed clock?) keep the table from " +
			"being split by day: fix the clock or delete those rows."
	case errors.Is(err, ErrBusy):
		return "Another pg_sage session is converting it."
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "55P03":
			return "A long transaction or a lock on the table kept pg_sage from its brief " +
				"exclusive lock: end long-running transactions, or restart pg_sage at a quiet time."
		case pgErr.Code == "57014":
			return "Validating the table took longer than " + convertStatementTimeout.String() +
				" (writers were not blocked); a quieter time or a smaller table will let it finish."
		case strings.HasPrefix(pgErr.Code, "53"):
			return "The database ran out of disk or memory: free disk space on the server."
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "pg_sage stopped or ran out of time before the conversion finished."
	}
	return "The PostgreSQL log shows the statement that failed."
}

// concurrentKeyMinBytes is the heap size from which the partitioned key is
// built CONCURRENTLY. A smaller table's key builds in well under a second
// under a SHARE lock (writers wait, within the lock timeout). CONCURRENTLY
// waits for every older snapshot in the database, which deadlocks when
// the converting session holds a lock another session waits for (bootstrap
// holds its advisory lock while a second instance waits for it); bootstrap
// only converts heaps up to this size.
const concurrentKeyMinBytes = 32 << 20

// keyBuild is "CONCURRENTLY " for a large heap, "" otherwise.
func keyBuild(ctx context.Context, s DB, t Table) (string, error) {
	var heap int64
	if err := s.QueryRow(ctx, "SELECT pg_catalog.pg_relation_size(pg_catalog.to_regclass($1))",
		t.regclass()).Scan(&heap); err != nil {
		return "", fmt.Errorf("read the heap size: %w", err)
	}
	if heap > concurrentKeyMinBytes {
		return "CONCURRENTLY ", nil
	}
	return "", nil
}
