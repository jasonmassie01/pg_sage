package executor

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/budget"
)

// Workload budget actions (market-08): an enforce-mode role budget's
// settings. set_role_timeout is ALTER ROLE r IN DATABASE d SET (or RESET)
// statement_timeout or idle_in_transaction_session_timeout; the
// cluster-wide set_role_connection_limit is ALTER ROLE r CONNECTION LIMIT
// n, HIGH risk, always approved by an operator and run by the fleet
// leader only.
const (
	ActionTypeSetRoleTimeout         = "set_role_timeout"
	ActionTypeSetRoleConnectionLimit = "set_role_connection_limit"
)

// ErrRoleBudgetRefused: the role budget change may not run (a refused
// role, a limit below current sessions, not the fleet leader) or the
// server refused it for lack of privilege (a managed provider's admin
// role); the error carries why, the server's message included.
var ErrRoleBudgetRefused = errors.New("role budget change refused")

// roleBudgetStmt is one parsed role budget statement.
type roleBudgetStmt struct {
	Role, Database string
	// Name and Value are the setting (Value "" for RESET).
	Name, Value string
	Reset       bool
	// ConnLimit marks ALTER ROLE ... CONNECTION LIMIT Limit.
	ConnLimit bool
	Limit     int
}

const roleIdent = `("(?:[^"]|"")*"|[A-Za-z_][A-Za-z0-9_$]*)`

const roleBudgetSetting = `(statement_timeout|idle_in_transaction_session_timeout)`

var (
	roleSettingPattern = regexp.MustCompile(`(?is)^ALTER\s+ROLE\s+` + roleIdent +
		`\s+IN\s+DATABASE\s+` + roleIdent + `\s+(?:SET\s+` + roleBudgetSetting +
		`\s*(?:=|\s+TO)\s*'([0-9]+(?:\.[0-9]+)?(?:ms|s|min|h)?)'|RESET\s+` +
		roleBudgetSetting + `)\s*;?$`)
	roleConnLimitPattern = regexp.MustCompile(`(?is)^ALTER\s+ROLE\s+` + roleIdent +
		`\s+(?:WITH\s+)?CONNECTION\s+LIMIT\s+(-?[0-9]{1,9})\s*;?$`)
)

// parseRoleBudgetSQL reads one of the two role budget statement forms.
func parseRoleBudgetSQL(sql string) (roleBudgetStmt, bool) {
	s := strings.TrimSpace(sql)
	if m := roleSettingPattern.FindStringSubmatch(s); m != nil {
		stmt := roleBudgetStmt{Role: identName(m[1]), Database: identName(m[2]),
			Name: strings.ToLower(m[3]), Value: m[4]}
		if m[5] != "" {
			stmt.Name, stmt.Reset = strings.ToLower(m[5]), true
		}
		return stmt, stmt.Role != "" && stmt.Database != "" && !bareKeyword(m[1])
	}
	if m := roleConnLimitPattern.FindStringSubmatch(s); m != nil {
		limit, err := strconv.Atoi(m[2])
		if err != nil || limit < -1 {
			return roleBudgetStmt{}, false
		}
		stmt := roleBudgetStmt{Role: identName(m[1]), ConnLimit: true, Limit: limit}
		return stmt, stmt.Role != "" && !bareKeyword(m[1])
	}
	return roleBudgetStmt{}, false
}

// roleKeywords are the role specifications ALTER ROLE accepts bare that
// name no single role (or the session's own).
var roleKeywords = map[string]bool{"all": true, "current_user": true, "session_user": true,
	"current_role": true, "public": true}

// bareKeyword reports an unquoted role keyword.
func bareKeyword(ident string) bool {
	return !strings.HasPrefix(ident, `"`) && roleKeywords[strings.ToLower(ident)]
}

// identName is the name an identifier denotes: a quoted one unquoted, a
// bare one folded to lower case as PostgreSQL does.
func identName(ident string) string {
	if strings.HasPrefix(ident, `"`) {
		return strings.ReplaceAll(ident[1:len(ident)-1], `""`, `"`)
	}
	return strings.ToLower(ident)
}

// roleBudgetActionType classifies a role budget statement ("" otherwise).
func roleBudgetActionType(sql string) string {
	stmt, ok := parseRoleBudgetSQL(sql)
	switch {
	case !ok:
		return ""
	case stmt.ConnLimit:
		return ActionTypeSetRoleConnectionLimit
	}
	return ActionTypeSetRoleTimeout
}

// isRoleBudgetSetting reports the settings a role budget may set.
func isRoleBudgetSetting(name string) bool {
	return name == budget.SettingStatementTimeout || name == budget.SettingIdleInTx
}

// checkRoleBudgetSQL is the validator's secondary check of ALTER ROLE.
func checkRoleBudgetSQL(sql string) error {
	if _, ok := parseRoleBudgetSQL(sql); !ok {
		return fmt.Errorf("%w: ALTER ROLE may only set or reset statement_timeout or "+
			"idle_in_transaction_session_timeout IN DATABASE, or set CONNECTION LIMIT",
			ErrDisallowedSQL)
	}
	return nil
}

// roleSettingRollback restores a role's prior setting in the database;
// a prior value the executor could not set back has no faithful rollback.
func roleSettingRollback(stmt roleBudgetStmt, prior *string) (string, error) {
	sql := budget.RoleSettingRollbackSQL(stmt.Role, stmt.Database, stmt.Name, prior)
	if _, ok := parseRoleBudgetSQL(sql); !ok {
		return "", fmt.Errorf("%w: role %s's prior %s %q is not a duration the executor "+
			"can set back", ErrNoFaithfulRollback, stmt.Role, stmt.Name, *prior)
	}
	return sql, nil
}

// roleBudgetExecError maps a permission refusal of the server (SQLSTATE
// 42501, a managed provider's limited admin role) to a refusal that
// carries the server's message; other errors pass through.
func roleBudgetExecError(sql string, err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42501" {
		msg := pgErr.Message
		if pgErr.Detail != "" {
			msg += " (" + pgErr.Detail + ")"
		}
		return fmt.Errorf("%w: the server refused %s for lack of privilege (SQLSTATE 42501): "+
			"%s; grant pg_sage's role CREATEROLE with ADMIN on the role, or apply it "+
			"through the provider", ErrRoleBudgetRefused, sql, msg)
	}
	return err
}

func setRoleTimeoutContract() ActionContract {
	return ActionContract{
		ActionType: ActionTypeSetRoleTimeout, BaseRiskTier: "moderate",
		ProviderSupport:     portableActionProviders(),
		RequiredPermissions: []string{"CREATEROLE with ADMIN on the role, or superuser"},
		Prechecks: []string{
			"enforce-mode role budget with 7 days of warn-mode shadow data",
			"no protected application would have been cancelled",
			"role is not a superuser, pg_sage's role, a pg_ role or a provider admin role",
			"IN DATABASE names the connected database",
		},
		Guardrails: []string{"cluster role lock", "dedicated transaction",
			"statement_timeout", "lock_timeout", "prior value re-read under the lock"},
		ExecutionPlan: []string{"ALTER ROLE r IN DATABASE d SET setting = 'value'"},
		SuccessCriteria: []string{"pg_db_role_setting holds the value for new sessions",
			roleTimeoutLogCheck},
		PostChecks:    []string{"read pg_db_role_setting back", roleTimeoutLogCheck},
		RollbackClass: "reversible", Cooldown: "role-scoped",
		AuditFields: []string{"case_id", "database", "role", "setting", "prior", "value"},
	}
}

func setRoleConnectionLimitContract() ActionContract {
	return ActionContract{
		ActionType: ActionTypeSetRoleConnectionLimit, BaseRiskTier: "high",
		ProviderSupport:     portableActionProviders(),
		RequiredPermissions: []string{"CREATEROLE with ADMIN on the role, or superuser"},
		Prechecks: []string{
			"enforce-mode role budget with 7 days of warn-mode shadow data",
			"limit is at least the role's current sessions plus a margin",
			"role is not a superuser, pg_sage's role, a pg_ role or a provider admin role",
			"this process is the fleet leader (the limit is cluster-wide)",
		},
		Guardrails: []string{"approval required", "cluster role lock",
			"dedicated transaction", "statement_timeout", "lock_timeout"},
		ExecutionPlan:   []string{"ALTER ROLE r CONNECTION LIMIT n"},
		SuccessCriteria: []string{"pg_roles.rolconnlimit is the limit"},
		PostChecks:      []string{"read pg_roles.rolconnlimit back"},
		RollbackClass:   "reversible", Cooldown: "role-scoped",
		AuditFields: []string{"case_id", "role", "prior_connection_limit",
			"connection_limit", "current_sessions"},
	}
}

// roleTimeoutLogCheck is the verification of an enforced statement_timeout
// (market-08b): its cancels, counted from the server log, must stay within
// twice the budget's shadow prediction for 24 hours, or it is reverted.
const roleTimeoutLogCheck = "statement-timeout cancels from the server log"
