package agentguard

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The kill's per-cluster steps (§6.10 steps 3-6): roles are cluster-wide,
// so they are disabled once per cluster, on pg_sage's pool on the
// cluster's first monitored database; backends are listed cluster-wide in
// pg_stat_activity.

// killScope is what one run covers on a cluster.
type killScope struct {
	actionType string
	scope      KillScope
	all        bool     // every agent role (RoleRegex), every principal
	ids        []string // principals (scope principal, or all when known)
	database   string   // scope database: the target's name
	reason     string
	actor      string
	killID     int64
}

// roles are the explicit role names of the scope's principals.
func (k killScope) roles() []string {
	out := make([]string, 0, 2*len(k.ids))
	for _, id := range k.ids {
		out = append(out, BrokerRoleName(id), LoginRoleName(id))
	}
	return out
}

// anyRole: the scope reaches every agent role's sessions (a fleet kill,
// or every agent's sessions in one database).
func (k killScope) anyRole() bool { return k.all || k.scope == KillScopeDatabase }

// disablesRoles: a database kill leaves the cluster-wide roles alone.
func (k killScope) disablesRoles() bool { return k.scope != KillScopeDatabase }

// roleAttrsSQL reads the agent roles in scope with their attributes.
const roleAttrsSQL = `/* pg_sage guard_kill v1 */
SELECT rolname::text, rolcanlogin, rolconnlimit FROM pg_catalog.pg_roles
WHERE rolname = ANY($1) OR ($2 AND rolname ~ '` + RoleRegex + `') ORDER BY 1`

type roleRow struct {
	name  string
	attrs RoleAttrs
}

func readRoleAttrs(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, names []string, all bool) ([]roleRow, error) {
	rows, err := q.Query(ctx, roleAttrsSQL, names, all)
	if err != nil {
		return nil, fmt.Errorf("agentguard: reading agent roles: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (roleRow, error) {
		var x roleRow
		return x, r.Scan(&x.name, &x.attrs.Login, &x.attrs.ConnectionLimit)
	})
	if err != nil {
		return nil, fmt.Errorf("agentguard: reading agent roles: %w", err)
	}
	return out, nil
}

// killed reports a role already in the kill's state; its attributes are
// not prior attributes.
func (a RoleAttrs) killed() bool { return !a.Login && a.ConnectionLimit == 0 }

// disableRole runs ALTER ROLE … NOLOGIN CONNECTION LIMIT 0 in its own
// transaction under the lock timeout, retrying a lock timeout or a
// concurrent catalog update.
func (s *Switch) disableRole(ctx context.Context, pool *pgxpool.Pool, role string) (
	string, error) {
	stmt := "ALTER ROLE " + ident(role) + " NOLOGIN CONNECTION LIMIT 0"
	var err error
	for attempt := 0; attempt < s.cfg.Attempts; attempt++ {
		err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, fmt.Sprintf("/* pg_sage guard_kill v1 */ "+
				"SET LOCAL lock_timeout = '%dms'", s.cfg.LockTimeout.Milliseconds())); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "/* pg_sage guard_kill v1 */ "+stmt)
			return err
		})
		if err == nil || !retryableRoleError(err) || ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		return stmt, fmt.Errorf("%s: %w", stmt, err)
	}
	return stmt, nil
}

// retryableRoleError is a lock timeout or a concurrent update of the role.
func retryableRoleError(err error) bool {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return false
	}
	return pg.Code == sqlLockNotAvailable || pg.Code == "40001" || pg.Code == "XX000"
}

// terminateSQL ends every agent backend in scope but pg_sage's own; an
// empty datname covers the cluster.
const terminateSQL = `/* pg_sage guard_kill v1 */
SELECT count(*) FILTER (WHERE pg_catalog.pg_terminate_backend(a.pid))::int
FROM pg_catalog.pg_stat_activity a
WHERE a.pid <> pg_catalog.pg_backend_pid()
  AND (a.usename = ANY($1) OR ($2 AND a.usename ~ '` + RoleRegex + `'))
  AND ($3 = '' OR a.datname = $3)`

// countAgentSQL counts the backends terminateSQL would end.
const countAgentSQL = `/* pg_sage guard_kill v1 */
SELECT count(*)::int FROM pg_catalog.pg_stat_activity a
WHERE a.pid <> pg_catalog.pg_backend_pid()
  AND (a.usename = ANY($1) OR ($2 AND a.usename ~ '` + RoleRegex + `'))
  AND ($3 = '' OR a.datname = $3)`

type rowQuery interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// terminate ends the agent backends visible on q's server.
func terminate(ctx context.Context, q rowQuery, k killScope, datname string) (int, error) {
	var n int
	if err := q.QueryRow(ctx, terminateSQL, k.roles(), k.anyRole(), datname).
		Scan(&n); err != nil {
		return 0, signalError("terminating agent backends", err)
	}
	return n, nil
}

func countAgents(ctx context.Context, q rowQuery, k killScope, datname string) (int, error) {
	var n int
	if err := q.QueryRow(ctx, countAgentSQL, k.roles(), k.anyRole(), datname).
		Scan(&n); err != nil {
		return 0, fmt.Errorf("agentguard: counting agent backends: %w", err)
	}
	return n, nil
}

// signalError names the grant pg_sage needs when it may not signal an
// agent backend (it holds SET, not INHERIT, on agent roles).
func signalError(what string, err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "42501" {
		return fmt.Errorf("agentguard: %s: %w; pg_sage's role needs pg_signal_backend: "+
			"GRANT pg_signal_backend TO <pg_sage role>", what, err)
	}
	return fmt.Errorf("agentguard: %s: %w", what, err)
}

// cancelApprovals marks open approvals of the scope cancelled_kill on one
// database: pending, or approved but not yet executed.
func cancelApprovals(ctx context.Context, pool *pgxpool.Pool, k killScope) (int, error) {
	all := k.anyRole()
	tag, err := pool.Exec(ctx, `/* pg_sage guard_kill v1 */
		UPDATE sage.action_queue SET status = 'cancelled_kill', decided_at = now(),
			reason = left('cancelled by the agent kill switch: ' || $3, 2000)
		WHERE principal_id IS NOT NULL AND status IN ('pending', 'approved')
		  AND action_log_id IS NULL AND ($1 OR principal_id = ANY($2))`,
		all, k.ids, k.reason)
	if err != nil {
		return 0, fmt.Errorf("agentguard: cancelling approvals: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// cancelInflight cancels the in-flight backends recorded for a database.
func cancelInflight(ctx context.Context, pool *pgxpool.Pool, pids []int) (int, error) {
	if len(pids) == 0 {
		return 0, nil
	}
	var n int
	err := pool.QueryRow(ctx, `/* pg_sage guard_kill v1 */
		SELECT count(*) FILTER (WHERE pg_catalog.pg_cancel_backend(p))::int
		FROM unnest($1::int[]) AS p
		WHERE p <> pg_catalog.pg_backend_pid()
		  AND EXISTS (SELECT 1 FROM pg_catalog.pg_stat_activity a WHERE a.pid = p)`,
		pids).Scan(&n)
	if err != nil {
		return 0, signalError("cancelling in-flight agent requests", err)
	}
	return n, nil
}

// datnameOf is the target's real database name (datname).
func datnameOf(ctx context.Context, q rowQuery) (string, error) {
	var name string
	if err := q.QueryRow(ctx, "/* pg_sage guard_kill v1 */ SELECT current_database()").
		Scan(&name); err != nil {
		return "", fmt.Errorf("agentguard: reading the database name: %w", err)
	}
	return name, nil
}

func joinErr(prev string, err error) string {
	if err == nil {
		return prev
	}
	if prev == "" {
		return err.Error()
	}
	return prev + "; " + err.Error()
}

func trimStatements(stmts []string) string { return strings.Join(stmts, ";\n") }
