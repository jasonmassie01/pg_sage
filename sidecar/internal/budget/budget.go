// Package budget is pg_sage's workload budgets (market-08): per role,
// application_name or sqlcommenter tag limits on statement time, idle in
// transaction time and connections, in the style of PlanetScale Traffic
// Control.
//
// A budget starts in warn mode: each collector cycle counts the
// statements it would have cancelled and the connections it would have
// rejected into sage.budget_shadow and raises an info finding. A role
// budget in enforce mode becomes ALTER ROLE actions only after 7 days of
// shadow data in which no protected application would have been
// cancelled; application_name and tag budgets stay warn-only, because
// PostgreSQL applies settings per role (the runaway policy is their
// enforcement path). Pool sizes are advisory text.
package budget

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/provider"
)

// Mode is a budget's mode.
type Mode string

// Budget modes.
const (
	ModeWarn    Mode = config.BudgetModeWarn
	ModeEnforce Mode = config.BudgetModeEnforce
)

// PromotionWindow is how much shadow data a budget needs before enforce
// mode acts.
const PromotionWindow = 7 * 24 * time.Hour

// Match selects the sessions a budget covers: exactly one field is set.
// Tag is "key=value".
type Match struct {
	Role, ApplicationName, Tag string
}

// Budget is one validated workload budget. A zero limit is no limit.
type Budget struct {
	Name                       string
	Match                      Match
	StatementTimeoutMs         int64
	IdleInTransactionTimeoutMs int64
	ConnectionLimit            int
	PoolSize                   int
	Mode                       Mode
}

// Enforceable reports whether the budget can become role settings.
func (b Budget) Enforceable() bool {
	return b.Mode == ModeEnforce && b.Match.Role != ""
}

// FromConfig converts the configured budgets; config.Validate has checked
// them, so an error here is a configuration that skipped validation.
func FromConfig(cfgs []config.BudgetConfig) ([]Budget, error) {
	out := make([]Budget, 0, len(cfgs))
	for _, c := range cfgs {
		stmt, err := config.ParseBudgetDuration(c.StatementTimeout)
		if err != nil {
			return nil, fmt.Errorf("budget %q statement_timeout: %w", c.Name, err)
		}
		idle, err := config.ParseBudgetDuration(c.IdleInTransactionSessionTimeout)
		if err != nil {
			return nil, fmt.Errorf("budget %q idle_in_transaction_session_timeout: %w",
				c.Name, err)
		}
		mode := Mode(c.Mode)
		if mode == "" {
			mode = ModeWarn
		}
		out = append(out, Budget{Name: c.Name, Match: Match{Role: c.Match.Role,
			ApplicationName: c.Match.ApplicationName, Tag: c.Match.Tag},
			StatementTimeoutMs: stmt, IdleInTransactionTimeoutMs: idle,
			ConnectionLimit: c.ConnectionLimit, PoolSize: c.PoolSize, Mode: mode})
	}
	return out, nil
}

var adminRoles = regexp.MustCompile(provider.AdminRolePattern)

// RefusedRole reports why no budget may cover role: the bootstrap
// superuser, a provider administration role, a reserved pg_ role,
// pg_sage's own role (selfRole) or any superuser. nil allows it.
func RefusedRole(role, selfRole string, superuser bool) error {
	switch {
	case role == "":
		return fmt.Errorf("a role budget needs a role")
	case role == selfRole:
		return fmt.Errorf("role %s is refused: it is pg_sage's own role", role)
	case role == "postgres":
		return fmt.Errorf("role postgres is refused: the bootstrap superuser is never budgeted")
	case adminRoles.MatchString(role):
		return fmt.Errorf("role %s is refused: it is a provider administration role", role)
	case strings.HasPrefix(role, "pg_"):
		return fmt.Errorf("role %s is refused: pg_ roles are reserved", role)
	case superuser:
		return fmt.Errorf("role %s is refused: a superuser is never budgeted", role)
	}
	return nil
}

// ProposeStatementTimeoutMs is the statement_timeout pg_sage proposes for
// a role: 3 times its p99 statement time, rounded up to a whole second,
// never below the longest statement observed. 0 (no proposal) without
// statement time.
func ProposeStatementTimeoutMs(p99Ms, observedMaxMs float64) int64 {
	if p99Ms <= 0 {
		return 0
	}
	ms := math.Max(3*p99Ms, observedMaxMs)
	return int64(math.Ceil(ms/1000)) * 1000
}

// ShadowDay is one day of a budget's shadow ledger.
type ShadowDay struct {
	Day              time.Time
	FirstSeen        time.Time
	WouldCancel      int
	WouldReject      int
	ProtectedCancels int
	WorstMs          float64
}

// CanPromote reports whether the shadow ledger allows enforce mode to
// act at now: the ledger must reach back PromotionWindow and record no
// would-cancel of a protected application. why explains a refusal.
func CanPromote(history []ShadowDay, now time.Time) (ok bool, why string) {
	if len(history) == 0 {
		return false, "no shadow data yet; enforce mode needs 7 days of warn-mode data"
	}
	first := history[0].FirstSeen
	protected := 0
	for _, d := range history {
		if d.FirstSeen.Before(first) {
			first = d.FirstSeen
		}
		protected += d.ProtectedCancels
	}
	if span := now.Sub(first); span < PromotionWindow {
		return false, fmt.Sprintf("only %s of shadow data; enforce mode needs 7 days",
			spanText(span))
	}
	if protected > 0 {
		return false, fmt.Sprintf("%d would-cancel(s) of a protected application in the "+
			"shadow window; enforce mode never cancels protected applications", protected)
	}
	return true, ""
}

// spanText renders a span as "6 days 23 hours".
func spanText(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	days := int(d / (24 * time.Hour))
	hours := int((d % (24 * time.Hour)) / time.Hour)
	return fmt.Sprintf("%d days %d hours", days, hours)
}
