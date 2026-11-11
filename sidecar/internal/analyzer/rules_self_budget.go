package analyzer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/selfbudget"
	"github.com/pg-sage/sidecar/internal/selfcost"
)

// categorySelfBudget is the finding raised when pg_sage exceeds its
// declared budget for itself (self_budget: database time and blocks per
// hour and sage schema size per database; the sidecar's CPU per cycle
// once for the process, on the SidecarObject).
const categorySelfBudget = "sage_self_budget"

// SidecarObject is the object type and identifier of the process CPU
// finding the process watch raises (footprint-07): the sidecar, not a
// database.
const SidecarObject = "sidecar"

// topConsumers is how many statements and loops a finding names.
const topConsumers = 5

// selfBudgetMeter keeps the loops' previous counters and the latest
// usage. The cycle goroutine writes it; /metrics reads the usage
// concurrently. The process CPU is not a database's: the process watch
// measures it (footprint-07).
type selfBudgetMeter struct {
	mu        sync.Mutex
	loopsPrev map[string]selfbudget.LoopStat
	usage     selfbudget.Usage
	loops     []selfbudget.LoopCost
}

func newSelfBudgetMeter() *selfBudgetMeter { return &selfBudgetMeter{} }

// observe records the cycle's usage (measured from the self-cost reading,
// plus the history store's share in history.store: meta) and returns it
// with database's loops of local work that were busiest since the
// previous cycle.
func (m *selfBudgetMeter) observe(u selfbudget.Usage, database string) (
	selfbudget.Usage, []selfbudget.LoopCost) {
	if m == nil {
		return u, nil
	}
	cur := selfbudget.ForDatabase(selfbudget.LocalWork(selfbudget.Process().Snapshot()),
		database)
	m.mu.Lock()
	defer m.mu.Unlock()
	loops := selfbudget.TopLoops(m.loopsPrev, cur, topConsumers)
	m.loopsPrev, m.usage, m.loops = cur, u, loops
	return u, loops
}

func (m *selfBudgetMeter) last() selfbudget.Usage {
	u, _ := m.lastWithLoops()
	return u
}

// lastWithLoops is the latest usage and a copy of the busiest loops.
func (m *selfBudgetMeter) lastWithLoops() (selfbudget.Usage, []selfbudget.LoopCost) {
	if m == nil {
		return selfbudget.Usage{}, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loops == nil {
		return m.usage, nil
	}
	return m.usage, append([]selfbudget.LoopCost(nil), m.loops...)
}

// trackedCycle runs a cycle as one run of the analyzer loop: its busy
// time for the budget and, when it analyzed nothing or could not publish,
// a failed run (observability-06).
func (a *Analyzer) trackedCycle(ctx context.Context) {
	ctx, done := a.loopTracker().TrackForErr(ctx, "analyzer")
	done(a.cycle(ctx))
}

// loopTracker is the process-wide loop tracker unless a test replaced it.
func (a *Analyzer) loopTracker() *selfbudget.Loops {
	if a.loops != nil {
		return a.loops
	}
	return selfbudget.Process()
}

// errNoSnapshot and errStaleSnapshot are cycles with no fresh evidence:
// the collector has no snapshot yet, or none since the last cycle.
var (
	errNoSnapshot    = errors.New("analyzer cycle skipped: no collector snapshot yet")
	errStaleSnapshot = errors.New("analyzer cycle skipped: no fresh collector snapshot")
)

// SelfBudgetUsage is pg_sage's latest measured usage on this database.
func (a *Analyzer) SelfBudgetUsage() selfbudget.Usage {
	return a.selfBudget.last()
}

// selfBudgetFor is the budget the analyzer checks on its database. The
// process CPU is not in it: the process watch checks it once for every
// database. Database time is only checked when self_budget sets it:
// inherited from analyzer.self_cost_budget_ms it is the sage_self_cost
// finding's.
func selfBudgetFor(cfg *config.Config) selfbudget.Budget {
	b := selfbudget.Budget{BlocksPerHour: float64(cfg.SelfBudget.BlocksPerHour),
		StorageBytes: int64(cfg.SelfBudget.StorageMB) << 20}
	if perHour, explicit := cfg.EffectiveSelfDBTimeMsPerHour(); explicit {
		b.DBTimeMsPerHour = perHour
	}
	return b
}

// checkSelfBudget measures the cycle's usage and raises the finding when
// pg_sage is over budget. The first cycle after a start knows no DB
// window: the category stays unknown so an open finding stays open. An
// evaluated cycle keeps the process watch's sidecar finding open: its
// absence from this database's findings is no evidence about the CPU.
func (a *Analyzer) checkSelfBudget(ctx context.Context, cost selfcost.Cost) []Finding {
	b := selfBudgetFor(a.cfg)
	hist, err := a.historyStorage(ctx)
	if err != nil {
		a.logFn("WARN", "analyzer: measure this database's share of the history store: %v",
			err)
	}
	u, loops := a.selfBudget.observe(withHistoryStorage(selfbudget.FromCost(cost), hist, err),
		loopDatabase(ctx, cost))
	if !budgetDecidable(b, u) {
		a.evalFail(categorySelfBudget)
		return nil
	}
	a.eval.evaluated(categorySelfBudget)
	a.eval.keep(categorySelfBudget, SidecarObject)
	return ruleSelfBudget(cost.Database, selfbudget.Check(b, u), a.selfCost.Top(), loops)
}

// loopDatabase is the name this database's loops are tracked under: the
// runtime's (fleet) name its ctx carries, else the database the cost was
// read from.
func loopDatabase(ctx context.Context, cost selfcost.Cost) string {
	if db := selfbudget.DatabaseOf(ctx); db != "" {
		return db
	}
	return cost.Database
}

// budgetDecidable is true when every enabled database resource was
// measured.
func budgetDecidable(b selfbudget.Budget, u selfbudget.Usage) bool {
	return ((b.DBTimeMsPerHour <= 0 && b.BlocksPerHour <= 0) || u.DBKnown) &&
		(b.StorageBytes <= 0 || u.StorageKnown)
}

// ruleSelfBudget raises one warning naming every resource over budget and
// the top consumers. Like sage_self_cost it is about pg_sage's bill on the
// database; the self-monitoring filter exempts its category, so it names
// pg_sage and shows statement texts as selfcost reports them.
func ruleSelfBudget(database string, breaches []selfbudget.Breach,
	top []selfcost.StatementCost, loops []selfbudget.LoopCost) []Finding {
	if len(breaches) == 0 {
		return nil
	}
	names := make([]string, 0, len(breaches))
	rows := make([]map[string]any, 0, len(breaches))
	for _, b := range breaches {
		names = append(names, string(b.Resource))
		rows = append(rows, map[string]any{"resource": string(b.Resource), "used": b.Used,
			"limit": b.Limit, "unit": b.Unit, "share": b.Share()})
	}
	return []Finding{{
		Category: categorySelfBudget, Severity: "warning",
		ObjectType: "database", ObjectIdentifier: database,
		Title: fmt.Sprintf("pg_sage monitoring exceeded its own %s budget on %s",
			strings.Join(names, ", "), database),
		Detail: map[string]any{"breaches": rows,
			"top_statements": statementRows(top), "top_loops": loopRows(loops)},
		Recommendation: selfBudgetAdvice(loops),
	}}
}

func statementRows(top []selfcost.StatementCost) []map[string]any {
	out := make([]map[string]any, 0, len(top))
	for _, s := range top {
		out = append(out, map[string]any{"text": s.Text, "component": s.Component,
			"db_time_ms": s.TimeMs, "calls": s.Calls, "blocks": s.Blocks})
	}
	return out
}

func loopRows(loops []selfbudget.LoopCost) []map[string]any {
	out := make([]map[string]any, 0, len(loops))
	for _, l := range loops {
		out = append(out, map[string]any{"loop": l.Name, "busy_ms": l.BusyMs,
			"runs": l.Runs})
	}
	return out
}

func selfBudgetAdvice(loops []selfbudget.LoopCost) string {
	busiest := ""
	if len(loops) > 0 {
		busiest = fmt.Sprintf("The busiest loop was %s (%.0f ms over %d runs). ",
			loops[0].Name, loops[0].BusyMs, loops[0].Runs)
	}
	return busiest + "top_statements and top_loops show where the cost went. " +
		"Lengthen collector.interval_seconds or analyzer.interval_seconds, turn off " +
		"components you do not use, shorten retention for storage, or raise the " +
		"self_budget value if this cost is expected."
}
