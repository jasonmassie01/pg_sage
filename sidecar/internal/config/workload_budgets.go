package config

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/provider"
)

// Workload budgets (market-08, docs/workload-budgets.md): per role,
// application_name or sqlcommenter tag limits that pg_sage measures in
// warn mode and, for role budgets promoted to enforce, applies as role
// settings. Budget modes.
const (
	BudgetModeWarn    = "warn"
	BudgetModeEnforce = "enforce"
)

// BudgetMatch selects the sessions a budget covers: exactly one field.
type BudgetMatch struct {
	Role            string `yaml:"role" doc:"Login role the budget covers. Only a role budget can be enforced: PostgreSQL applies settings and connection limits per role."`
	ApplicationName string `yaml:"application_name" doc:"application_name the budget covers (exact match). Warn-only by design; the runaway policy enforces per application."`
	Tag             string `yaml:"tag" doc:"sqlcommenter tag key=value the budget covers, read from long-running active queries. Warn-only by design."`
}

// BudgetConfig is one workload budget.
type BudgetConfig struct {
	Name                            string      `yaml:"name" doc:"Unique budget name, the key of its shadow ledger and findings."`
	Match                           BudgetMatch `yaml:"match"`
	StatementTimeout                string      `yaml:"statement_timeout" doc:"Longest statement the budget allows, a PostgreSQL duration (250ms, 6s, 2min, 1h; a bare number is ms). Empty or 0: none."`
	IdleInTransactionSessionTimeout string      `yaml:"idle_in_transaction_session_timeout" doc:"Longest idle in transaction wait the budget allows, a PostgreSQL duration. Empty or 0: none."`
	ConnectionLimit                 int         `yaml:"connection_limit" doc:"Most client sessions of the role, cluster-wide (role budgets only). 0: none. Enforcing it is HIGH risk and always needs approval."`
	PoolSize                        int         `yaml:"pool_size" doc:"Suggested PgBouncer pool size for the workload. Advisory text only: the pooler's configuration is outside the database. 0: none."`
	Mode                            string      `yaml:"mode" doc:"warn (default) counts would-be violations in sage.budget_shadow; enforce (role budgets only) proposes the role settings after 7 days of shadow data."`
}

// budgetDurationPattern is a non-negative PostgreSQL duration in ms, s,
// min or h; a bare number is milliseconds.
var budgetDurationPattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*(ms|s|min|h)?$`)

var budgetUnitMs = map[string]float64{"": 1, "ms": 1, "s": 1000, "min": 60000, "h": 3600000}

// ParseBudgetDuration converts a budget duration to whole milliseconds;
// "" and "0" are 0 (no limit). A fraction of a millisecond is refused.
func ParseBudgetDuration(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	m := budgetDurationPattern.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("%q is not a non-negative duration (250ms, 6s, 2min, 1h)", s)
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("%q: %w", s, err)
	}
	ms := n * budgetUnitMs[m[2]]
	if ms != math.Trunc(ms) || ms > math.MaxInt32 {
		return 0, fmt.Errorf("%q must be whole milliseconds at most %d", s, math.MaxInt32)
	}
	return int64(ms), nil
}

// budgetRefusedRoles are roles no budget may cover: the bootstrap
// superuser and the managed-service administration logins.
var budgetRefusedRoles = regexp.MustCompile(provider.AdminRolePattern)

// validateBudgets checks the budgets list. pg_sage's own role and
// superusers are refused at run time, when they are known.
func validateBudgets(budgets []BudgetConfig) error {
	seen := map[string]bool{}
	for i, b := range budgets {
		if strings.TrimSpace(b.Name) == "" {
			return fmt.Errorf("budgets[%d]: name is required", i)
		}
		if seen[b.Name] {
			return fmt.Errorf("budgets[%d]: duplicate budget name %q", i, b.Name)
		}
		seen[b.Name] = true
		if err := b.validate(); err != nil {
			return fmt.Errorf("budgets[%d] %q: %w", i, b.Name, err)
		}
	}
	return nil
}

func (b BudgetConfig) validate() error {
	if err := b.Match.validate(); err != nil {
		return err
	}
	stmt, err := ParseBudgetDuration(b.StatementTimeout)
	if err != nil {
		return fmt.Errorf("statement_timeout: %w", err)
	}
	idle, err := ParseBudgetDuration(b.IdleInTransactionSessionTimeout)
	if err != nil {
		return fmt.Errorf("idle_in_transaction_session_timeout: %w", err)
	}
	switch {
	case b.ConnectionLimit < 0:
		return fmt.Errorf("connection_limit must be 0 (none) or positive, got %d",
			b.ConnectionLimit)
	case b.PoolSize < 0:
		return fmt.Errorf("pool_size must be 0 (none) or positive, got %d", b.PoolSize)
	case b.Mode != "" && b.Mode != BudgetModeWarn && b.Mode != BudgetModeEnforce:
		return fmt.Errorf("mode must be warn or enforce, got %q", b.Mode)
	case stmt == 0 && idle == 0 && b.ConnectionLimit == 0 && b.PoolSize == 0:
		return fmt.Errorf("the budget sets no limit: set statement_timeout, " +
			"idle_in_transaction_session_timeout, connection_limit or pool_size")
	case b.Match.Role == "" && b.ConnectionLimit > 0:
		return fmt.Errorf("connection_limit is a role limit: match a role to set it")
	case b.Match.Role == "" && b.Mode == BudgetModeEnforce:
		return fmt.Errorf("application_name and tag budgets are warn-only: PostgreSQL " +
			"enforces settings per role (the runaway policy covers applications)")
	}
	return nil
}

func (m BudgetMatch) validate() error {
	set := 0
	for _, v := range []string{m.Role, m.ApplicationName, m.Tag} {
		if v != "" {
			set++
		}
	}
	if set != 1 {
		return fmt.Errorf("match must set exactly one of role, application_name or tag")
	}
	if m.Tag != "" {
		if k, v, ok := strings.Cut(m.Tag, "="); !ok || k == "" || v == "" {
			return fmt.Errorf("match.tag must be key=value, got %q", m.Tag)
		}
	}
	if m.Role == "postgres" {
		return fmt.Errorf("match.role postgres is refused: the bootstrap superuser " +
			"is never budgeted")
	}
	if budgetRefusedRoles.MatchString(m.Role) {
		return fmt.Errorf("match.role %q is refused: it is a provider administration role",
			m.Role)
	}
	return nil
}
