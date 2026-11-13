package budget

import (
	"fmt"
	"math"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// The role settings a budget enforces, as GUC names.
const (
	SettingStatementTimeout = "statement_timeout"
	SettingIdleInTx         = "idle_in_transaction_session_timeout"
)

// RoleSettingSQL is the enforce statement of one role timeout in one
// database: ALTER ROLE r IN DATABASE d SET name = '<ms>ms'.
func RoleSettingSQL(role, database, name string, ms int64) string {
	return "ALTER ROLE " + pgx.Identifier{role}.Sanitize() + " IN DATABASE " +
		pgx.Identifier{database}.Sanitize() + " SET " + name + " = " +
		sanitize.QuoteLiteral(strconv.FormatInt(ms, 10)+"ms")
}

// RoleSettingRollbackSQL restores the role's prior setting in the
// database: SET it back when there was one, RESET otherwise.
func RoleSettingRollbackSQL(role, database, name string, prior *string) string {
	prefix := "ALTER ROLE " + pgx.Identifier{role}.Sanitize() + " IN DATABASE " +
		pgx.Identifier{database}.Sanitize()
	if prior == nil {
		return prefix + " RESET " + name
	}
	return prefix + " SET " + name + " = " + sanitize.QuoteLiteral(*prior)
}

// ConnectionLimitSQL sets a role's cluster-wide connection limit (-1 is
// none).
func ConnectionLimitSQL(role string, limit int) string {
	return fmt.Sprintf("ALTER ROLE %s CONNECTION LIMIT %d", pgx.Identifier{role}.Sanitize(),
		limit)
}

// ConnectionLimitMargin is the headroom a connection limit must leave
// over the role's current sessions: 2 sessions or 10%, the larger.
func ConnectionLimitMargin(current int) int {
	return max(2, int(math.Ceil(float64(current)*0.1)))
}

// CheckConnectionLimit refuses a limit that would reject sessions the
// role holds now: it must be at least current sessions plus the margin.
func CheckConnectionLimit(limit, current int) error {
	if need := current + ConnectionLimitMargin(current); limit < need {
		return fmt.Errorf("connection_limit %d is below the role's %d current sessions plus "+
			"a margin of %d: it would reject live connections (raise it to at least %d)",
			limit, current, ConnectionLimitMargin(current), need)
	}
	return nil
}

// settingMs reads a stored role setting value as milliseconds; ok is
// false when it is not a duration pg_sage understands.
func settingMs(v string) (int64, bool) {
	ms, err := config.ParseBudgetDuration(v)
	return ms, err == nil
}
