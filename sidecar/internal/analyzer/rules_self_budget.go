package analyzer

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/selfbudget"
	"github.com/pg-sage/sidecar/internal/selfcost"
)

// categorySelfBudget is the finding raised when pg_sage exceeds its
// declared budget for itself (self_budget: sidecar CPU per cycle,
// database time and blocks per hour, sage schema size).
const categorySelfBudget = "sage_self_budget"

// topConsumers is how many statements and loops a finding names.
const topConsumers = 5

// selfBudgetMeter keeps the sidecar CPU meter, the loops' previous
// counters and the latest usage. The cycle goroutine writes it; /metrics
// reads the usage concurrently.
type selfBudgetMeter struct {
	cpu *selfbudget.CPUMeter

	mu        sync.Mutex
	loopsPrev map[string]selfbudget.LoopStat
	usage     selfbudget.Usage
}

func newSelfBudgetMeter() *selfBudgetMeter {
	return &selfBudgetMeter{cpu: selfbudget.NewCPUMeter(selfbudget.ProcessCPU, time.Now)}
}

// observe records the cycle's usage and returns it with the loops that
// were busiest since the previous cycle.
func (m *selfBudgetMeter) observe(cost selfcost.Cost, cycle time.Duration) (
	selfbudget.Usage, []selfbudget.LoopCost) {
	u := selfbudget.FromCost(cost)
	if m == nil {
		return u, nil
	}
	u.CPUMsPerCycle, u.CPUKnown = m.cpu.Observe(cycle)
	cur := selfbudget.Process().Snapshot()
	m.mu.Lock()
	defer m.mu.Unlock()
	loops := selfbudget.TopLoops(m.loopsPrev, cur, topConsumers)
	m.loopsPrev, m.usage = cur, u
	return u, loops
}

func (m *selfBudgetMeter) last() selfbudget.Usage {
	if m == nil {
		return selfbudget.Usage{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.usage
}

// trackedCycle runs a cycle and records its busy time for the budget.
func (a *Analyzer) trackedCycle(ctx context.Context) {
	done := selfbudget.Process().Track("analyzer")
	a.cycle(ctx)
	done()
}

// SelfBudgetUsage is pg_sage's latest measured usage on this database.
func (a *Analyzer) SelfBudgetUsage() selfbudget.Usage {
	return a.selfBudget.last()
}

// selfBudgetFor is the budget the analyzer checks. Database time is only
// checked when self_budget sets it: inherited from
// analyzer.self_cost_budget_ms it is the sage_self_cost finding's.
func selfBudgetFor(cfg *config.Config) selfbudget.Budget {
	b := selfbudget.Budget{CPUMsPerCycle: float64(cfg.SelfBudget.CPUMsPerCycle),
		BlocksPerHour: float64(cfg.SelfBudget.BlocksPerHour),
		StorageBytes:  int64(cfg.SelfBudget.StorageMB) << 20}
	if perHour, explicit := cfg.EffectiveSelfDBTimeMsPerHour(); explicit {
		b.DBTimeMsPerHour = perHour
	}
	return b
}

// checkSelfBudget measures the cycle's usage and raises the finding when
// pg_sage is over budget. The first cycle after a start knows no CPU or
// DB window: the category stays unknown so an open finding stays open.
func (a *Analyzer) checkSelfBudget(cost selfcost.Cost) []Finding {
	b := selfBudgetFor(a.cfg)
	u, loops := a.selfBudget.observe(cost, a.collectorInterval())
	if !budgetDecidable(b, u) {
		a.evalFail(categorySelfBudget)
		return nil
	}
	a.eval.evaluated(categorySelfBudget)
	return ruleSelfBudget(cost.Database, selfbudget.Check(b, u), a.selfCost.Top(), loops)
}

// budgetDecidable is true when every enabled resource was measured.
func budgetDecidable(b selfbudget.Budget, u selfbudget.Usage) bool {
	return (b.CPUMsPerCycle <= 0 || u.CPUKnown) &&
		((b.DBTimeMsPerHour <= 0 && b.BlocksPerHour <= 0) || u.DBKnown) &&
		(b.StorageBytes <= 0 || u.StorageKnown)
}

// ruleSelfBudget raises one warning naming every resource over budget and
// the top consumers. Like sage_self_cost it is about the database's bill:
// nothing in it may name pg_sage (the self-monitoring filter would drop
// it), so statement texts are shown with the tag removed and the name
// redacted.
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
		Title: fmt.Sprintf("Sage monitoring exceeded its own %s budget on %s",
			strings.Join(names, ", "), database),
		Detail: map[string]any{"breaches": rows,
			"top_statements": statementRows(top), "top_loops": loopRows(loops)},
		Recommendation: selfBudgetAdvice(loops),
	}}
}

func statementRows(top []selfcost.StatementCost) []map[string]any {
	out := make([]map[string]any, 0, len(top))
	for _, s := range top {
		out = append(out, map[string]any{"text": redactSelf(s.Text),
			"db_time_ms": s.TimeMs, "calls": s.Calls, "blocks": s.Blocks})
	}
	return out
}

func loopRows(loops []selfbudget.LoopCost) []map[string]any {
	out := make([]map[string]any, 0, len(loops))
	for _, l := range loops {
		out = append(out, map[string]any{"loop": redactSelf(l.Name), "busy_ms": l.BusyMs,
			"runs": l.Runs})
	}
	return out
}

// selfName is the sidecar's own name in any case.
var selfName = regexp.MustCompile(`(?i)pg_sage`)

// redactSelf keeps the finding clear of the self-monitoring filter.
func redactSelf(s string) string {
	return selfName.ReplaceAllString(s, "sidecar")
}

func selfBudgetAdvice(loops []selfbudget.LoopCost) string {
	busiest := ""
	if len(loops) > 0 {
		busiest = fmt.Sprintf("The busiest loop was %s (%.0f ms over %d runs). ",
			redactSelf(loops[0].Name), loops[0].BusyMs, loops[0].Runs)
	}
	return busiest + "top_statements and top_loops show where the cost went. " +
		"Lengthen collector.interval_seconds or analyzer.interval_seconds, turn off " +
		"components you do not use, shorten retention for storage, or raise the " +
		"self_budget value if this cost is expected."
}
