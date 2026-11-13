package budget

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// Finding categories.
const (
	// CategoryViolation (info): a budget would have cancelled or rejected
	// work today.
	CategoryViolation = "workload_budget_violation"
	// CategoryProposal (info): a statement_timeout budget proposed for a
	// role without one.
	CategoryProposal = "workload_budget_proposal"
	// CategoryEnforce: an enforce-mode role budget's setting, as a
	// set_role_timeout or set_role_connection_limit action.
	CategoryEnforce = "workload_budget_enforce"
)

// proposalWindow is how far back a role's statement time is read for a
// proposal; proposalMinCalls the statements it must have run.
const (
	proposalWindow   = 14 * 24 * time.Hour
	proposalMinCalls = 100
	maxProposals     = 20
)

// SnapshotSource is the collector's latest snapshot.
type SnapshotSource interface {
	LatestSnapshot() *collector.Snapshot
}

// RoleEvidence is a role's statement time over the proposal window.
type RoleEvidence struct {
	P99Ms, MaxMs float64
	Calls        int64
}

// Ledger is the shadow ledger and the per-role evidence (Store).
type Ledger interface {
	Record(ctx context.Context, day time.Time, tallies []Tally) error
	RecordRoles(ctx context.Context, day time.Time, roles []collector.WorkloadRoleStats) error
	// History is the rows of one budget definition (Budget.Definition).
	History(ctx context.Context, budget, definition string, since time.Time) ([]ShadowDay,
		error)
	RoleEvidence(ctx context.Context, since time.Time) (map[string]RoleEvidence, error)
}

// RoleFact is what enforcement reads about a role: superuser, its
// cluster-wide connection limit (-1: none) and its settings in this
// database (pg_db_role_setting, name to value).
type RoleFact struct {
	Superuser bool
	ConnLimit int
	Settings  map[string]string
}

// RoleCatalog reads roles on the monitored database (Catalog).
type RoleCatalog interface {
	SelfRole(ctx context.Context) (string, error)
	CurrentDatabase(ctx context.Context) (string, error)
	RoleFacts(ctx context.Context, roles []string) (map[string]RoleFact, error)
}

// Monitor is the workload budget detector the analyzer runs each cycle.
type Monitor struct {
	cfg      *config.Config
	src      SnapshotSource
	ledger   Ledger
	catalog  RoleCatalog
	database string
	eval     *Evaluator
	logFn    func(string, string, ...any)
	now      func() time.Time
	// evaluated is the categories the last Detect fully evaluated.
	evaluated []string
	// cancels counts statement-timeout cancels from the server log (nil:
	// no log source); cancelLedger records them and reads reverts (nil
	// when the ledger keeps neither) (market-08b).
	cancels      atomic.Pointer[CancelCounter]
	cancelLedger CancelLedger
}

// NewMonitor builds the detector for one database (its ledger key).
func NewMonitor(cfg *config.Config, src SnapshotSource, ledger Ledger, catalog RoleCatalog,
	database string, logFn func(string, string, ...any)) *Monitor {
	cancelLedger, _ := ledger.(CancelLedger)
	return &Monitor{cfg: cfg, src: src, ledger: ledger, catalog: catalog, database: database,
		eval: NewEvaluator(), logFn: logFn, now: time.Now, cancelLedger: cancelLedger}
}

// LastEvaluatedCategories is the categories the last Detect evaluated, so
// their cleared findings resolve.
func (m *Monitor) LastEvaluatedCategories() []string {
	return append([]string(nil), m.evaluated...)
}

// Detect measures the latest workload reading against the budgets,
// records the shadow ledger and returns the findings. Without a reading
// (no snapshot, or an unknown workload_sessions category) it evaluates
// nothing, so open findings stay open.
func (m *Monitor) Detect(ctx context.Context) ([]analyzer.Finding, error) {
	m.evaluated = nil
	snap := m.src.LatestSnapshot()
	if snap == nil || snap.WorkloadSessions == nil || !snap.Available("workload_sessions") {
		return nil, nil
	}
	set := m.settings()
	budgets, err := FromConfig(set.budgets)
	if err != nil {
		return nil, err
	}
	now := m.now()
	day := now.UTC().Truncate(24 * time.Hour)
	tallies := m.eval.Evaluate(budgets, snap.WorkloadSessions, set.protected,
		snap.CollectedAt)
	if err := m.record(ctx, day, tallies, snap.WorkloadSessions.Roles); err != nil {
		return nil, err
	}
	if err := m.recordCancels(ctx, day, budgets); err != nil {
		return nil, err
	}
	out, err := m.budgetFindings(ctx, budgets, snap.WorkloadSessions, now)
	if err != nil {
		return nil, err
	}
	proposals, err := m.proposals(ctx, budgets, now, set.proposals)
	if err != nil {
		return nil, err
	}
	m.evaluated = []string{CategoryViolation, CategoryEnforce, CategoryProposal}
	return append(out, proposals...), nil
}

// settings is this cycle's copy of the configuration the monitor reads,
// taken under the hot-reload lock.
type settings struct {
	budgets   []config.BudgetConfig
	protected []string
	proposals bool
}

func (m *Monitor) settings() settings {
	config.RLockForHotReload()
	defer config.RUnlockForHotReload()
	return settings{budgets: append([]config.BudgetConfig(nil), m.cfg.Budgets...),
		protected: append([]string(nil), m.cfg.Runaway.SafePatterns...),
		proposals: m.cfg.Advisor.BudgetProposals}
}

func (m *Monitor) record(ctx context.Context, day time.Time, tallies []Tally,
	roles []collector.WorkloadRoleStats) error {
	if len(tallies) > 0 {
		if err := m.ledger.Record(ctx, day, tallies); err != nil {
			return fmt.Errorf("record the budget shadow ledger: %w", err)
		}
	}
	if err := m.ledger.RecordRoles(ctx, day, roles); err != nil {
		return fmt.Errorf("record per-role statement time: %w", err)
	}
	return nil
}

// budgetFindings raises each budget's violation finding and, for an
// enforce-mode role budget past its promotion gate, its actions.
func (m *Monitor) budgetFindings(ctx context.Context, budgets []Budget,
	ws *collector.WorkloadSessions, now time.Time) ([]analyzer.Finding, error) {
	var out []analyzer.Finding
	for _, b := range budgets {
		history, err := m.ledger.History(ctx, b.Name, b.Definition(),
			now.Add(-PromotionWindow-24*time.Hour))
		if err != nil {
			return nil, fmt.Errorf("read the shadow ledger of budget %q: %w", b.Name, err)
		}
		if f, ok := violationFinding(b, history, now); ok {
			out = append(out, f)
		}
		if !b.Enforceable() {
			continue
		}
		ok, why, err := m.promotable(ctx, b, history, now)
		if err != nil {
			return nil, err
		}
		if !ok {
			m.logFn("INFO", "budget %q stays in warn mode: %s", b.Name, why)
			continue
		}
		actions, err := m.enforceActions(ctx, b, ws, history, now)
		if err != nil {
			return nil, err
		}
		out = append(out, actions...)
	}
	return out, nil
}
