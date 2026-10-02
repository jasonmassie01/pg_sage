package earned

import (
	"context"
	"time"
)

// Downgrade reasons (CHECK-40). While any holds, a pair acts at most at L1.
const (
	DowngradeBudgetBurn         = "error_budget_fast_burn"
	DowngradeBudgetUnknown      = "error_budget_unknown"
	DowngradeBudgetUnavailable  = "error_budget_unavailable"
	DowngradeHARole             = "ha_role_not_primary"
	DowngradeFailover           = "failover_in_progress"
	DowngradeStaleEvidence      = "evidence_stale"
	DowngradeConcurrent         = "concurrent_action"
	DowngradeConcurrencyUnknown = "concurrency_unknown"
	DowngradeSafetyRegression   = "family_safety_regression"
)

// Downgrade is one active downgrade signal.
type Downgrade struct {
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// BudgetState is the error-budget state of a database's SLOs.
type BudgetState struct {
	// Configured is false when the database has no SLO; there is then no
	// budget that could burn.
	Configured bool
	// FastBurning reports a page-level (fast window) burn.
	FastBurning bool
	// Unknown reports an SLO whose burn cannot be computed (no data, a
	// zero denominator, a stale evaluation).
	Unknown bool
	Detail  string
}

// BudgetSource reads error-budget state. Sage SRE M5's SLO engine
// provides it (its BudgetSummary); an adapter is wired at integration.
type BudgetSource interface {
	ErrorBudget(ctx context.Context, database string) (BudgetState, error)
}

// HA roles, as internal/ha reports them.
const (
	RolePrimary = "primary"
	RoleReplica = "replica"
	RoleUnknown = "unknown"
)

// HAState is the node's HA state at authorization time.
type HAState struct {
	Role           string
	SafeMode       bool
	LastRoleChange time.Time
}

// HASource reads the HA state of the database.
type HASource interface {
	HAStatus(ctx context.Context) (HAState, error)
}

// ConcurrencySource counts other pg_sage actions on the targets: active
// change leases (unless leaseHeld: the caller holds its own) and actions
// executed within window.
type ConcurrencySource interface {
	ConcurrentActions(ctx context.Context, targets []string, leaseHeld bool,
		window time.Duration) (int, error)
}
