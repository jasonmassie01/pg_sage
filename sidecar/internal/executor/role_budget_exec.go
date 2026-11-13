package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/budget"
	"github.com/pg-sage/sidecar/internal/clusterlock"
)

const configKindRoleBudget = "role_budget"

// roleLockWait bounds the wait for another process's change of the role.
const roleLockWait = 30 * time.Second

// roleBudgetPrior is a role's state before a role budget change: its
// setting in the database (nil: none) or its connection limit.
type roleBudgetPrior struct {
	Role      string  `json:"role"`
	Database  string  `json:"database,omitempty"`
	Setting   string  `json:"setting,omitempty"`
	Value     *string `json:"value,omitempty"`
	ConnLimit *int    `json:"connection_limit,omitempty"`
}

// roleFacts is what the prechecks read about the role and the session.
type roleFacts struct {
	exists, superuser bool
	connLimit         int
	sessions          int
	self, database    string
	setting           *string
}

// roleFactsSQL reads the role, its cluster-wide client sessions, its
// setting in the connected database and who pg_sage is.
const roleFactsSQL = `/* pg_sage executor */
SELECT r.oid IS NOT NULL, COALESCE(r.rolsuper, false), COALESCE(r.rolconnlimit, -1),
       (SELECT count(*)::int FROM pg_stat_activity
         WHERE usename = $1 AND backend_type = 'client backend'),
       session_user::text, current_database()::text,
       (SELECT substring(c FROM length($2) + 2)
          FROM pg_db_role_setting s CROSS JOIN LATERAL unnest(s.setconfig) c
         WHERE s.setrole = r.oid
           AND s.setdatabase = (SELECT oid FROM pg_database WHERE datname = current_database())
           AND split_part(c, '=', 1) = $2)
  FROM (SELECT 1) one LEFT JOIN pg_roles r ON r.rolname = $1`

func readRoleFacts(ctx context.Context, q pgx.Tx, role, setting string) (roleFacts, error) {
	var f roleFacts
	err := q.QueryRow(ctx, roleFactsSQL, role, setting).Scan(&f.exists, &f.superuser,
		&f.connLimit, &f.sessions, &f.self, &f.database, &f.setting)
	return f, err
}

// prepareRoleBudget runs the prechecks and captures the prior state and
// the rollback of a role budget change into c. Refusals happen here,
// before anything changes.
func (e *Executor) prepareRoleBudget(ctx context.Context, c *configChange,
	stmt roleBudgetStmt) error {
	db := e.actionDB() // the identity that will run the change (security-09)
	if db == nil {
		return fmt.Errorf("%w: database pool unavailable", ErrConfigPrior)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: role %s: %v", ErrConfigPrior, stmt.Role, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	facts, err := readRoleFacts(ctx, tx, stmt.Role, stmt.Name)
	if err != nil {
		return fmt.Errorf("%w: role %s: %v", ErrConfigPrior, stmt.Role, err)
	}
	if err := e.checkRoleBudget(stmt, facts); err != nil {
		return err
	}
	prior, rollback, err := roleBudgetRollback(stmt, facts)
	if err != nil {
		return err
	}
	c.kind, c.role, c.priorRole, c.rollbackSQL = configKindRoleBudget, stmt, prior, rollback
	return nil
}

// checkRoleBudget refuses a role no budget may change, another database,
// a connection limit below current sessions plus a margin, and a
// connection limit off the fleet leader.
func (e *Executor) checkRoleBudget(stmt roleBudgetStmt, f roleFacts) error {
	if !f.exists {
		return fmt.Errorf("%w: role %s does not exist", ErrRoleBudgetRefused, stmt.Role)
	}
	if err := budget.RefusedRole(stmt.Role, f.self, f.superuser); err != nil {
		return fmt.Errorf("%w: %v", ErrRoleBudgetRefused, err)
	}
	if monitor := e.monitoringRole(); monitor != "" && stmt.Role == monitor {
		return fmt.Errorf("%w: role %s is refused: it is pg_sage's monitoring role",
			ErrRoleBudgetRefused, stmt.Role)
	}
	if !stmt.ConnLimit {
		if stmt.Database != f.database {
			return fmt.Errorf("%w: IN DATABASE %s is not the connected database %s",
				ErrRoleBudgetRefused, stmt.Database, f.database)
		}
		return nil
	}
	if e.isLeader != nil && !e.isLeader() {
		return fmt.Errorf("%w: a connection limit is cluster-wide; only the fleet leader "+
			"applies it", ErrRoleBudgetRefused)
	}
	if stmt.Limit >= 0 {
		if err := budget.CheckConnectionLimit(stmt.Limit, f.sessions); err != nil {
			return fmt.Errorf("%w: %v", ErrRoleBudgetRefused, err)
		}
	}
	return nil
}

func roleBudgetRollback(stmt roleBudgetStmt, f roleFacts) (roleBudgetPrior, string, error) {
	prior := roleBudgetPrior{Role: stmt.Role}
	if stmt.ConnLimit {
		limit := f.connLimit
		prior.ConnLimit = &limit
		return prior, budget.ConnectionLimitSQL(stmt.Role, limit), nil
	}
	prior.Database, prior.Setting, prior.Value = stmt.Database, stmt.Name, f.setting
	rollback, err := roleSettingRollback(stmt, f.setting)
	return prior, rollback, err
}

// execRoleBudget runs a prepared role budget change: under the cluster
// role lock (two databases or processes changing one role take turns),
// in one transaction with statement_timeout and lock_timeout, it re-reads
// the prior state and refuses to run when it changed since the rollback
// in beforeState was captured, then runs the statement.
func (e *Executor) execRoleBudget(ctx context.Context, sql string, beforeState map[string]any,
	timeout time.Duration, lockTimeoutMS int) error {
	if err := ValidateExecutorSQL(sql); err != nil {
		return fmt.Errorf("SQL validation: %w", err)
	}
	stmt, _ := parseRoleBudgetSQL(sql)
	want := preparedRollback(beforeState)
	if want == "" {
		return fmt.Errorf("%w: no captured rollback for %s", ErrNoFaithfulRollback, sql)
	}
	release, err := e.roleLock(ctx, stmt.Role)
	if err != nil {
		return fmt.Errorf("%w: cluster lock for role %s: %v", ErrRoleBudgetRefused,
			stmt.Role, err)
	}
	defer release()
	return roleBudgetExecError(sql, e.runRoleBudgetTx(ctx, stmt, sql, want, timeout,
		lockTimeoutMS))
}

func (e *Executor) runRoleBudgetTx(ctx context.Context, stmt roleBudgetStmt, sql, want string,
	timeout time.Duration, lockTimeoutMS int) error {
	tx, err := e.actionDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d",
		timeout.Milliseconds())); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = %d",
		max(lockTimeoutMS, 1))); err != nil {
		return err
	}
	facts, err := readRoleFacts(ctx, tx, stmt.Role, stmt.Name)
	if err != nil {
		return fmt.Errorf("%w: re-read role %s: %v", ErrConfigPrior, stmt.Role, err)
	}
	if err := e.checkRoleBudget(stmt, facts); err != nil {
		return err
	}
	if _, now, err := roleBudgetRollback(stmt, facts); err != nil || now != want {
		return fmt.Errorf("%w: role %s changed since its prior state was captured "+
			"(rollback now %q, captured %q)", ErrConfigPrior, stmt.Role, now, want)
	}
	if _, err := tx.Exec(ctx, sql); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// monitoringRole is the login of pg_sage's monitoring pool when changes run
// as a separate action identity: the precheck's session_user is then the
// action role, so the monitoring role is refused by name. "" without one.
func (e *Executor) monitoringRole() string {
	if e.actionPool == nil || e.pool == nil {
		return ""
	}
	return e.pool.Config().ConnConfig.User
}

// preparedRollback is the rollback prepareConfigChange recorded.
func preparedRollback(beforeState map[string]any) string {
	change, _ := beforeState["config_change"].(map[string]any)
	if change == nil || change["kind"] != configKindRoleBudget {
		return ""
	}
	sql, _ := change["rollback_sql"].(string)
	return sql
}

// clusterRoleLock takes the cluster-wide lock of role in the server's
// maintenance database, with the pool's own connection settings.
func (e *Executor) clusterRoleLock(ctx context.Context, role string) (func(), error) {
	if e.pool == nil {
		return nil, errors.New("database pool unavailable")
	}
	lockCtx, cancel := context.WithTimeout(ctx, roleLockWait)
	defer cancel()
	return clusterlock.LockConfig(lockCtx, e.pool.Config().ConnConfig, "role budget:"+role)
}

// readRoleBudget reads a role budget change back from the catalog: the
// setting in pg_db_role_setting, or pg_roles.rolconnlimit.
func (e *Executor) readRoleBudget(ctx context.Context, c *configChange) (readbackState,
	string, map[string]any) {
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return readbackUnconfirmed, "role read-back failed: " + err.Error(),
			map[string]any{"state": readbackUnconfirmed}
	}
	defer func() { _ = tx.Rollback(ctx) }()
	f, err := readRoleFacts(ctx, tx, c.role.Role, c.role.Name)
	if err != nil {
		return readbackUnconfirmed, "role read-back failed: " + err.Error(),
			map[string]any{"state": readbackUnconfirmed}
	}
	detail := map[string]any{"role": c.role.Role, "setting": f.setting,
		"connection_limit": f.connLimit}
	ok := c.role.ConnLimit && f.connLimit == c.role.Limit ||
		!c.role.ConnLimit && c.role.Reset && f.setting == nil ||
		!c.role.ConnLimit && !c.role.Reset && f.setting != nil && *f.setting == c.role.Value
	state, note := readbackNotInEffect, "role "+c.role.Role+" does not show the change"
	if ok {
		state, note = readbackInEffect, "role "+c.role.Role+" holds the change for new sessions"
	}
	detail["state"] = state
	return state, note, detail
}

// WithLeaderCheck gates cluster-wide role changes (a connection limit)
// on the fleet leader; without one the process is its own leader.
func (e *Executor) WithLeaderCheck(isLeader func() bool) { e.isLeader = isLeader }
