package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// exposureLockTimeoutMS is the revoke's and the RLS enable's lock_timeout
// ceiling: both take a short lock on the table and must never queue
// behind a long transaction.
const exposureLockTimeoutMS = 2000

// exposureStateKey is where an exposure action's before_state records the
// object, its prior ACL and what was revoked.
const exposureStateKey = "exposure"

// exposureTarget is a table as an exposure action reads it.
type exposureTarget struct {
	relkind     string
	owner       bool     // pg_sage's action role owns it or is a member of the owner
	publicPrivs []string // PUBLIC's privileges, sorted (acldefault when relacl is NULL)
	acl         []string // relacl, or the default ACL when NULL
	rls         bool
}

const exposureTargetSQL = `/* pg_sage executor */ SELECT c.relkind::text,
  pg_catalog.pg_has_role(current_user, c.relowner, 'USAGE'),
  COALESCE((SELECT array_agg(DISTINCT a.privilege_type::text ORDER BY
      a.privilege_type::text)
    FROM pg_catalog.aclexplode(COALESCE(c.relacl,
      pg_catalog.acldefault('r', c.relowner))) a WHERE a.grantee = 0), '{}'),
  COALESCE(c.relacl, pg_catalog.acldefault('r', c.relowner))::text[],
  c.relrowsecurity
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relname = $2`

type exposureQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// readExposureTarget reads stmt's table; a missing relation, a view or any
// other non-table is not an exposure target.
func readExposureTarget(
	ctx context.Context, q exposureQuerier, stmt exposureStmt,
) (exposureTarget, error) {
	var t exposureTarget
	err := q.QueryRow(ctx, exposureTargetSQL, stmt.schema, stmt.table).Scan(
		&t.relkind, &t.owner, &t.publicPrivs, &t.acl, &t.rls)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, fmt.Errorf("%w: %s does not exist", ErrNotExposureTarget, stmt.qualified())
	}
	if err != nil {
		return t, fmt.Errorf("read the ACL of %s: %w", stmt.qualified(), err)
	}
	if t.relkind != "r" && t.relkind != "p" {
		return t, fmt.Errorf("%w: %s is not a table (relkind %s)", ErrNotExposureTarget,
			stmt.qualified(), t.relkind)
	}
	if !t.owner {
		return t, fmt.Errorf("%w (%s)", ErrExposureNotOwner, stmt.qualified())
	}
	return t, nil
}

// exposureNoChange reports a statement that would change nothing.
func exposureNoChange(stmt exposureStmt, t exposureTarget) bool {
	if stmt.kind == exposureEnableRLS {
		return t.rls
	}
	return len(revokablePrivileges(stmt.privileges, t.publicPrivs)) == 0
}

// execExposure runs one exposure remediation in one transaction under the
// lock_timeout ceiling: it re-reads the table, revokes only what PUBLIC
// holds (or enables RLS), checks the result and rolls everything back when
// the check fails. On success f carries the statement that ran and its
// exact rollback, and beforeState the prior ACL and what was revoked.
func (e *Executor) execExposure(
	ctx context.Context, f *analyzer.Finding, stmt exposureStmt,
	beforeState map[string]any, lockMS int,
) error {
	if err := exposureTargetRefusal(*f, stmt); err != nil {
		return err
	}
	if lockMS <= 0 || lockMS > exposureLockTimeoutMS {
		lockMS = exposureLockTimeoutMS
	}
	var sql, rollback string
	var revoked []string
	var prior []string
	o := applyDDLOpts([]DDLOption{WithLockTimeout(lockMS)})
	o.before = func(ctx context.Context, tx pgx.Tx) error {
		// ACCESS SHARE conflicts only with ACCESS EXCLUSIVE (DDL): the table
		// cannot be dropped or altered between the read and the check, and
		// a DDL holding it ends this run at lock_timeout.
		if _, err := tx.Exec(ctx, "LOCK TABLE "+stmt.qualified()+
			" IN ACCESS SHARE MODE"); err != nil {
			return exposureExecError(stmt, err)
		}
		t, err := readExposureTarget(ctx, tx, stmt)
		if err != nil {
			return err
		}
		if exposureNoChange(stmt, t) {
			return ErrExposureNoChange
		}
		prior = t.acl
		sql, rollback, revoked = exposureStatements(stmt, t)
		if _, err := tx.Exec(ctx, sql); err != nil {
			return exposureExecError(stmt, err)
		}
		return exposurePostCheck(ctx, tx, stmt, revoked)
	}
	timeout := e.ddlTimeoutFor(f.RecommendedSQL)
	if err := execTransaction(ctx, e.actionDB(), timeout, o, nil); err != nil {
		return err
	}
	f.RecommendedSQL, f.RollbackSQL = sql, rollback
	if beforeState != nil {
		beforeState[exposureStateKey] = map[string]any{"object": stmt.qualified(),
			"prior_acl": prior, "revoked": revoked, "action_type": stmt.actionType()}
	}
	return nil
}

// exposureStatements is the statement that runs and its rollback: the
// revoke narrowed to what PUBLIC holds, re-granted exactly; or RLS on/off.
func exposureStatements(stmt exposureStmt, t exposureTarget) (string, string, []string) {
	if stmt.kind == exposureEnableRLS {
		return rowSecuritySQL(stmt.schema, stmt.table, "ENABLE"),
			rowSecuritySQL(stmt.schema, stmt.table, "DISABLE"), nil
	}
	privs := revokablePrivileges(stmt.privileges, t.publicPrivs)
	return revokeSQL(stmt.schema, stmt.table, privs),
		regrantSQL(stmt.schema, stmt.table, privs), privs
}

// exposureExecError names the table on a lock timeout (retryable) and maps
// insufficient privilege to the permanent ownership refusal.
func exposureExecError(stmt exposureStmt, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42501" {
		return fmt.Errorf("%w (%s): %w", ErrExposureNotOwner, stmt.qualified(), err)
	}
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
		return fmt.Errorf("%w: %s does not exist", ErrNotExposureTarget, stmt.qualified())
	}
	if IsLockNotAvailable(err) {
		return fmt.Errorf("%w: lock on table %s not granted within lock_timeout; "+
			"retried next cycle: %w", ErrLockNotAvailable, stmt.qualified(), err)
	}
	return fmt.Errorf("%s on %s: %w", stmt.actionType(), stmt.qualified(), err)
}

// exposurePostCheck re-reads the table inside the transaction: PUBLIC must
// hold none of the revoked privileges (another grantor's grant survives a
// revoke by this role), and an RLS enable must read back on.
func exposurePostCheck(
	ctx context.Context, tx pgx.Tx, stmt exposureStmt, revoked []string,
) error {
	after, err := readExposureTarget(ctx, tx, stmt)
	if err != nil {
		return fmt.Errorf("post-check: %w", err)
	}
	if stmt.kind == exposureEnableRLS {
		if !after.rls {
			return fmt.Errorf("post-check: RLS on %s reads off", stmt.qualified())
		}
		return nil
	}
	if left := revokablePrivileges(revoked, after.publicPrivs); len(left) > 0 {
		return fmt.Errorf("%w: PUBLIC still holds %v on %s (granted by another role)",
			ErrExposureNotOwner, left, stmt.qualified())
	}
	return nil
}

// precheckExposure is the read-only check before a remediation is gated:
// whether there is anything to change, and the target refusals.
func (e *Executor) precheckExposure(
	ctx context.Context, stmt exposureStmt,
) (exposureTarget, error) {
	t, err := readExposureTarget(ctx, e.actionDB(), stmt)
	if err != nil {
		return t, err
	}
	if exposureNoChange(stmt, t) {
		return t, ErrExposureNoChange
	}
	return t, nil
}

// recordExposureRemediation stores what an executed revoke took away,
// keyed by its action row. A failed write is logged: the stored re-GRANT
// in rollback_sql still restores PUBLIC's access.
func (e *Executor) recordExposureRemediation(
	ctx context.Context, actionID int64, beforeState map[string]any,
) {
	state, _ := beforeState[exposureStateKey].(map[string]any)
	if e.pool == nil || actionID <= 0 || state == nil ||
		state["action_type"] != ActionTypeRevokePublicGrant {
		return
	}
	revoked, _ := state["revoked"].([]string)
	prior, _ := state["prior_acl"].([]string)
	if _, err := e.pool.Exec(ctx, `/* pg_sage executor */ INSERT INTO
		sage.exposure_remediations (action_log_id, object, prior_acl, revoked)
		VALUES ($1, $2, COALESCE(to_jsonb($3::text[]), '[]'),
		        COALESCE(to_jsonb($4::text[]), '[]'))
		ON CONFLICT (action_log_id) DO NOTHING`, actionID, state["object"],
		prior, revoked); err != nil {
		e.logFn("executor", "record exposure remediation of action %d: %v", actionID, err)
	}
}
