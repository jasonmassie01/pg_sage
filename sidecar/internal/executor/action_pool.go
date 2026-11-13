package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrActionIdentityUnavailable reports that the separate action identity
// (action_dsn) cannot be reached: the change is not run on the
// observation role instead, which lacks the rights by design.
var ErrActionIdentityUnavailable = errors.New("action identity unavailable")

// ActionPoolMaxConns sizes the action identity's pool: every DDL slot may
// hold two connections at once (a CONCURRENTLY build and its gate, or a
// watched VACUUM and its cancel), and one more stays free for backend
// cancel and terminate, so the kill switch never waits behind a long
// build. A two-connection pool deadlocked three concurrent builds.
func ActionPoolMaxConns() int32 { return 2*maxConcurrentDDL + 1 }

// actionIdentityProbe bounds the reachability check of the action pool
// before a change runs.
const actionIdentityProbe = 5 * time.Second

// WithActionPool gives the executor a second, acting identity
// (security-09): the changes Apply runs, their rollbacks and backend
// cancel/terminate use it; reads and pg_sage's own sage writes stay on the
// observation pool. Nil keeps the single identity.
func (e *Executor) WithActionPool(p *pgxpool.Pool) *Executor {
	e.actionPool = p
	return e
}

// HasActionIdentity reports whether changes run as a separate role.
func (e *Executor) HasActionIdentity() bool { return e.actionPool != nil }

// actionDB is the pool that changes and backend signals run on: the
// action identity when configured, else the one pool.
func (e *Executor) actionDB() *pgxpool.Pool {
	if e.actionPool != nil {
		return e.actionPool
	}
	return e.pool
}

// checkActionIdentity confirms the action identity answers before a
// change runs. Without one it is a no-op; a cancelled caller is reported
// as its own cancellation.
func (e *Executor) checkActionIdentity(ctx context.Context) error {
	if e.actionPool == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("check the action identity: %w", err)
	}
	pctx, cancel := context.WithTimeout(ctx, actionIdentityProbe)
	defer cancel()
	if err := e.actionPool.Ping(pctx); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("check the action identity: %w", ctx.Err())
		}
		return fmt.Errorf("%w: the action_dsn role does not answer (%v); the change "+
			"was not run, observation continues", ErrActionIdentityUnavailable, err)
	}
	return nil
}
