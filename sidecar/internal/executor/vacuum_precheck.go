package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Blocked reasons of the maintenance-privilege precheck (vacuum-06): the
// connected role lacks the privilege, or it could not be read.
const (
	reasonCapabilityMissing    = "capability_missing"
	reasonCapabilityUnverified = "capability_unverified"
)

// ErrMaintenancePrivilegeMissing marks a withhold because the connected role
// lacks the privilege to VACUUM or ALTER TABLE the table (capability_missing):
// an operator fixes it with a grant, so the manual-action API answers 409.
var ErrMaintenancePrivilegeMissing = errors.New("connected role lacks the maintenance privilege")

// maintainMinVersion is the first server_version_num with the MAINTAIN
// privilege and the pg_maintain role; naming MAINTAIN on an older server
// fails with "unrecognized privilege type".
const maintainMinVersion = 170000

// maintenancePrivilegeTimeout bounds the precheck's catalog reads.
const maintenancePrivilegeTimeout = 5 * time.Second

// maintenancePrivilege is the precheck's verdict for one table: allowed
// and on what basis, or the remedy an operator applies.
type maintenancePrivilege struct {
	Allowed bool
	// Exists is false when the table is not found; such an action is
	// allowed to run and fail with PostgreSQL's own error.
	Exists bool
	Basis  string // owner, superuser, database_owner or maintain
	Role   string
	Detail string
}

// maintenanceBaseSQL reads every check below PostgreSQL 17 in one catalog
// read: table owner (membership counts), superuser, database owner, the
// server version and the role. The table is $1.
const maintenanceBaseSQL = `/* pg_sage executor */ SELECT c.oid IS NOT NULL,
	COALESCE(pg_has_role(current_user, c.relowner, 'USAGE'), false),
	r.rolsuper, pg_has_role(current_user, d.datdba, 'USAGE'),
	current_setting('server_version_num')::int, current_user::text
	FROM pg_roles r
	CROSS JOIN pg_database d
	LEFT JOIN pg_class c ON c.oid = to_regclass($1)
	WHERE r.rolname = current_user AND d.datname = current_database()`

// maintainSQL reads the PostgreSQL 17+ VACUUM privilege: MAINTAIN on the
// table or membership of pg_maintain. It is issued on 17+ only.
const maintainSQL = `/* pg_sage executor */ SELECT has_table_privilege(to_regclass($1), 'MAINTAIN')
	OR pg_has_role('pg_maintain', 'USAGE')`

// readMaintenancePrivilege decides whether the connected role may run
// actionType on table. vacuum_table needs the owner, a superuser, the
// database owner, or on 17+ MAINTAIN or pg_maintain; set_table_autovacuum
// (ALTER TABLE) needs the owner or a superuser, as MAINTAIN does not
// cover ALTER TABLE.
func readMaintenancePrivilege(
	ctx context.Context, q rowQuerier, table, actionType string,
) (maintenancePrivilege, error) {
	table = strings.TrimSpace(table)
	if table == "" {
		return maintenancePrivilege{Detail: "the statement must name exactly one " +
			"table for its maintenance privilege to be checked: run one statement " +
			"per table"}, nil
	}
	var p maintenancePrivilege
	var owner, super, dbOwner bool
	var version int
	if err := q.QueryRow(ctx, maintenanceBaseSQL, table).Scan(&p.Exists, &owner, &super,
		&dbOwner, &version, &p.Role); err != nil {
		return maintenancePrivilege{}, fmt.Errorf(
			"read maintenance privilege of %s: %w", table, err)
	}
	switch {
	case !p.Exists:
		p.Allowed = true
	case super:
		p.Allowed, p.Basis = true, "superuser"
	case owner:
		p.Allowed, p.Basis = true, "owner"
	case actionType != "vacuum_table":
		p.Detail = fmt.Sprintf("ownership required: ALTER TABLE %s needs its owner or a "+
			"superuser; MAINTAIN does not cover ALTER TABLE (connected as %s)", table, p.Role)
	case dbOwner:
		p.Allowed, p.Basis = true, "database_owner"
	case version < maintainMinVersion:
		p.Detail = fmt.Sprintf("ownership required: VACUUM %s needs the table owner, the "+
			"database owner or a superuser (connected as %s)", table, p.Role)
	default:
		return readMaintain(ctx, q, table, p)
	}
	return p, nil
}

// readMaintain completes p with the PostgreSQL 17+ MAINTAIN read.
func readMaintain(
	ctx context.Context, q rowQuerier, table string, p maintenancePrivilege,
) (maintenancePrivilege, error) {
	var maintain bool
	if err := q.QueryRow(ctx, maintainSQL, table).Scan(&maintain); err != nil {
		return maintenancePrivilege{}, fmt.Errorf(
			"read MAINTAIN privilege of %s: %w", table, err)
	}
	if maintain {
		p.Allowed, p.Basis = true, "maintain"
		return p, nil
	}
	role := pgx.Identifier{p.Role}.Sanitize()
	p.Detail = fmt.Sprintf("grant MAINTAIN: GRANT MAINTAIN ON TABLE %s TO %s, or "+
		"GRANT pg_maintain TO %s, or make %s the table owner", table, role, role, role)
	return p, nil
}

// maintenanceActionType is the prechecked action type of sql, or "".
func maintenanceActionType(sql string) string {
	switch actionType := ActionTypeForSQL(sql); actionType {
	case "vacuum_table", "set_table_autovacuum":
		return actionType
	}
	return ""
}

// admitMaintenance is the vacuum_table and set_table_autovacuum precheck
// Apply runs after the first authorization: a role that cannot run the
// statement is withheld (capability_missing, with the grant that fixes
// it), and a precheck that cannot be read withholds too
// (capability_unverified). Both are recorded on the authorizing decision.
// A permitted VACUUM then passes the running-vacuum admission (vacuum-05).
func (e *Executor) admitMaintenance(
	ctx context.Context, sql string, decision ActionPolicyDecision,
) error {
	actionType := maintenanceActionType(sql)
	if actionType == "" {
		return nil
	}
	table := maintenanceTable(sql)
	// The privilege that matters is the role that will run the statement.
	if e.actionDB() == nil {
		return e.withholdMaintenance(ctx, decision, reasonCapabilityUnverified,
			"database pool unavailable to read the maintenance privilege", nil)
	}
	readCtx, cancel := context.WithTimeout(ctx, maintenancePrivilegeTimeout)
	defer cancel()
	p, err := readMaintenancePrivilege(readCtx, e.actionDB(), table, actionType)
	switch {
	case err != nil:
		return e.withholdMaintenance(ctx, decision, reasonCapabilityUnverified,
			err.Error(), err)
	case !p.Allowed:
		return e.withholdMaintenance(ctx, decision, reasonCapabilityMissing, p.Detail,
			ErrMaintenancePrivilegeMissing)
	case actionType == "vacuum_table" && p.Exists:
		return e.admitVacuumProgress(ctx, table, decision)
	}
	return nil
}

// withholdMaintenance records the withheld precheck on the authorizing
// decision and returns the *WithheldError, wrapping cause when there is one.
func (e *Executor) withholdMaintenance(
	ctx context.Context, decision ActionPolicyDecision, reason, detail string, cause error,
) error {
	e.recordMaintenanceWithheld(ctx, decision.DecisionID, reason, detail)
	decision.Decision, decision.BlockedReason, decision.Detail =
		PolicyDecisionBlocked, reason, detail
	withheld := &WithheldError{Decision: decision}
	if cause != nil {
		return &causedWithheldError{WithheldError: withheld, cause: cause}
	}
	return withheld
}

// causedWithheldError is a withhold caused by an error; its message is the
// withhold's, whose detail already names the cause.
type causedWithheldError struct {
	*WithheldError
	cause error
}

func (e *causedWithheldError) Unwrap() []error {
	return []error{e.WithheldError, e.cause}
}

// recordMaintenanceWithheld turns the authorizing decision into the block
// it became, keeping the gate's reason in the evidence. A blocked decision
// holds no budget slot and no object.
func (e *Executor) recordMaintenanceWithheld(
	ctx context.Context, decisionID int64, reason, detail string,
) {
	if e.pool == nil || decisionID <= 0 {
		return
	}
	if _, err := e.pool.Exec(ctx, `/* pg_sage executor */ UPDATE sage.decision
		SET verdict = 'blocked', reason = $2,
		    evidence = evidence || jsonb_build_object('maintenance_privilege',
		        jsonb_build_object('reason', $2::text, 'detail', $3::text,
		                           'gate_reason', reason))
		WHERE id = $1 AND verdict = 'execute'`, decisionID, reason, detail); err != nil {
		e.logFn("executor", "record withheld maintenance precheck on decision %d "+
			"(%s: %s): %v", decisionID, reason, detail, err)
	}
}
