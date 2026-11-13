package executor

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/budget"
)

// market-08b: a role budget rollback (the automatic revert of an enforced
// timeout) runs under the role's cluster lock, as the forward change did,
// and names a privilege refusal; other rollbacks take no lock.

// No concurrent access test: the lock itself is the executor's cluster
// role lock, covered by TestRoleBudget_TwoDatabasesSerialize; here it is
// a recording fake.

type lockRecorder struct {
	events  []string
	lockErr error
	execErr error
}

func (r *lockRecorder) config() RollbackMonitorConfig {
	return RollbackMonitorConfig{
		RoleLock: func(_ context.Context, role string) (func(), error) {
			r.events = append(r.events, "lock "+role)
			if r.lockErr != nil {
				return nil, r.lockErr
			}
			return func() { r.events = append(r.events, "unlock "+role) }, nil
		},
		execRollback: func(_ context.Context, sql string, _ time.Duration,
			_ ...DDLOption) error {
			r.events = append(r.events, "exec "+sql)
			return r.execErr
		},
	}
}

// Happy path through the rollback path itself (executeRollbackSQL).
func TestExecRollbackStatement_RoleBudgetRunsUnderTheRoleLock(t *testing.T) {
	sql := budget.RoleSettingRollbackSQL("app", "shop", budget.SettingStatementTimeout, nil)
	r := &lockRecorder{}
	reason, err := executeRollbackSQL(context.Background(), nil, sql, r.config())
	if err != nil || !strings.Contains(reason, "automatic rollback") {
		t.Fatalf("rollback: %q, %v", reason, err)
	}
	want := []string{"lock app", "exec " + sql, "unlock app"}
	if !slices.Equal(r.events, want) {
		t.Fatalf("events = %q, want %q", r.events, want)
	}
}

// Error propagation: a lock that cannot be taken refuses the rollback
// before it runs; a 42501 is named as a privilege refusal.
func TestExecRollbackStatement_RoleBudgetErrors(t *testing.T) {
	sql := budget.RoleSettingRollbackSQL("app", "shop", budget.SettingStatementTimeout, nil)
	r := &lockRecorder{lockErr: errors.New("lock wait timed out")}
	err := r.config().execRollbackStatement(context.Background(), nil, sql, time.Second)
	if !errors.Is(err, ErrRoleBudgetRefused) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("lock failure: %v, want a refusal naming the cause", err)
	}
	if slices.Contains(r.events, "exec "+sql) {
		t.Fatalf("the rollback ran without its lock: %q", r.events)
	}
	r = &lockRecorder{execErr: &pgconn.PgError{Code: "42501",
		Message: "permission denied to alter role"}}
	err = r.config().execRollbackStatement(context.Background(), nil, sql, time.Second)
	if !errors.Is(err, ErrRoleBudgetRefused) || !strings.Contains(err.Error(), "42501") ||
		!strings.Contains(err.Error(), "permission denied to alter role") {
		t.Fatalf("42501: %v, want a privilege refusal with the server message", err)
	}
	if want := []string{"lock app", "exec " + sql, "unlock app"}; !slices.Equal(r.events,
		want) {
		t.Fatalf("events = %q, want the lock released after the failure %q", r.events, want)
	}
}

// Other rollbacks take no role lock, and their errors pass through.
func TestExecRollbackStatement_OtherRollbacksTakeNoLock(t *testing.T) {
	sql := "DROP INDEX CONCURRENTLY IF EXISTS public.idx_orders_customer"
	r := &lockRecorder{execErr: errors.New("lock timeout")}
	err := r.config().execRollbackStatement(context.Background(), nil, sql, time.Second)
	if err == nil || errors.Is(err, ErrRoleBudgetRefused) || err.Error() != "lock timeout" {
		t.Fatalf("error = %v, want the statement's own error", err)
	}
	if want := []string{"exec " + sql}; !slices.Equal(r.events, want) {
		t.Fatalf("events = %q, want %q", r.events, want)
	}
	// Nil: without a RoleLock a role budget rollback still runs.
	cfg := r.config()
	cfg.RoleLock = nil
	r.events, r.execErr = nil, nil
	role := budget.RoleSettingRollbackSQL("app", "shop", budget.SettingStatementTimeout, nil)
	if err := cfg.execRollbackStatement(context.Background(), nil, role,
		time.Second); err != nil || !slices.Equal(r.events, []string{"exec " + role}) {
		t.Fatalf("no RoleLock: err %v events %q", err, r.events)
	}
}
