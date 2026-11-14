package schema

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ci-15 triage: a migration step that ends on a deadline says which one
// expired. No concurrent-access tests: namedDeadline is a pure function of
// its arguments, and one bootstrap session runs the steps in order.

// pgTimeout is the shape pgconn gives a query cut off by its context.
func pgTimeout(ctx context.Context) error {
	return fmt.Errorf("timeout: %w", ctx.Err())
}

func expired(t *testing.T, parent context.Context, cause error) context.Context {
	t.Helper()
	var ctx context.Context
	var cancel context.CancelFunc
	if cause == nil {
		ctx, cancel = context.WithTimeout(parent, time.Nanosecond)
	} else {
		ctx, cancel = context.WithTimeoutCause(parent, time.Nanosecond, cause)
	}
	t.Cleanup(cancel)
	<-ctx.Done()
	return ctx
}

func TestNamedDeadline_StepBudgetWhenTheCallerIsAlive(t *testing.T) {
	caller := context.Background()
	step := expired(t, caller, nil)
	err := namedDeadline(caller, fmt.Errorf("migration step s: commit: %w", pgTimeout(step)))
	want := "(deadline: the step budget of " + migrationStepTimeout.String() + " expired)"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want it to name %q", err, want)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v lost context.DeadlineExceeded", err)
	}
}

func TestNamedDeadline_TheCallersOwnContext(t *testing.T) {
	caller := expired(t, context.Background(), nil)
	err := namedDeadline(caller, pgTimeout(caller))
	want := "(deadline: the context the bootstrap was called with expired, " +
		"not a bootstrap budget)"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

func TestNamedDeadline_ABootstrapBudget(t *testing.T) {
	caller := expired(t, context.Background(),
		budgetExpired{what: "migration list", budget: 90 * time.Second})
	err := namedDeadline(caller, pgTimeout(caller))
	want := "(deadline: the migration list budget of 1m30s expired)"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

// Nil/empty and invalid input: no error, an error that is not a timeout,
// and a cancellation pass through unchanged.
func TestNamedDeadline_LeavesOtherErrorsAlone(t *testing.T) {
	if err := namedDeadline(context.Background(), nil); err != nil {
		t.Fatalf("nil error became %v", err)
	}
	plain := errors.New("permission denied for schema sage")
	if err := namedDeadline(context.Background(), plain); err != plain {
		t.Fatalf("plain error became %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	err := fmt.Errorf("begin: %w", canceled.Err())
	if got := namedDeadline(canceled, err); got != err {
		t.Fatalf("cancellation became %v", got)
	}
}

// blockingStepDB is a session whose every query waits for its context and
// then fails as pgconn does.
type blockingStepDB struct{ fakeStepDB }

func (b *blockingStepDB) QueryRow(ctx context.Context, _ string, _ ...any) pgx.Row {
	<-ctx.Done()
	return fakeRow{err: pgTimeout(ctx)}
}

// Error propagation through the runner: the caller's short deadline is
// named, with the step, in the error runMigrationSteps returns.
func TestRunMigrationSteps_NamesTheCallersDeadline(t *testing.T) {
	_ = captureLog(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	db := &blockingStepDB{}
	_, err := runMigrationSteps(ctx, db, []migrationStep{{name: "stepSlow", sql: testColumnStep}})
	if err == nil {
		t.Fatal("a step cut off by the caller's deadline succeeded")
	}
	for _, want := range []string{"migration step stepSlow", "the context the bootstrap " +
		"was called with expired"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want %q", err, want)
		}
	}
}

// Boundary: the list budget is the sum of the step budgets, capped, and
// names itself; a list at the cap says so.
func TestListBudget_NamesTheListBudget(t *testing.T) {
	if got := listBudget(3).Error(); got != "the migration list budget of 30s" {
		t.Fatalf("listBudget(3) = %q", got)
	}
	capped := listBudget(10_000)
	if capped.budget != migrationBootCap ||
		capped.Error() != "the migration list budget of "+migrationBootCap.String() {
		t.Fatalf("listBudget(10000) = %+v %q, want the boot cap", capped, capped.Error())
	}
	if listBudget(0).budget != 0 {
		t.Fatalf("listBudget(0) = %+v, want zero", listBudget(0))
	}
}

// causeDB is a session whose every call waits for its context, records the
// context's cause, and fails as pgconn does.
type causeDB struct{ cause error }

func (c *causeDB) wait(ctx context.Context) error {
	<-ctx.Done()
	c.cause = context.Cause(ctx)
	return pgTimeout(ctx)
}

func (c *causeDB) Exec(ctx context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, c.wait(ctx)
}

func (c *causeDB) Query(ctx context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, c.wait(ctx)
}

func (c *causeDB) QueryRow(ctx context.Context, _ string, _ ...any) pgx.Row {
	return fakeRow{err: c.wait(ctx)}
}

func (c *causeDB) Begin(ctx context.Context) (pgx.Tx, error) { return nil, c.wait(ctx) }

// The storage migration's own bound carries its cause, so a step it ends
// names the storage budget, not the caller's context (ci-15 review).
func TestMigrateStorage_ItsBudgetIsTheCause(t *testing.T) {
	saved := storageMigrationBudget
	t.Cleanup(func() { storageMigrationBudget = saved })
	storageMigrationBudget.budget = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db := &causeDB{}
	if err := migrateStorage(ctx, db); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("migrateStorage on a stalled session = %v, want a deadline", err)
	}
	var b budgetExpired
	if !errors.As(db.cause, &b) || b.what != "storage migration" {
		t.Fatalf("the stalled call's context cause = %v, want the storage budget", db.cause)
	}
	if saved.budget != storageMigrationTimeout {
		t.Fatalf("storageMigrationBudget = %s, want %s", saved.budget, storageMigrationTimeout)
	}
}
