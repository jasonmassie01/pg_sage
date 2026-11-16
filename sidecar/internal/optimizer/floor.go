package optimizer

import (
	"context"

	"github.com/pg-sage/sidecar/internal/collector"
)

// FloorSource is detail.source on the findings the deterministic index
// floor produces, so the case and the ledger show which path proposed it.
const FloorSource = "deterministic_floor"

// The floor's bounds: at most floorMaxPerTable candidates per table and
// floorMaxPerCycle admissions per cycle.
const (
	floorMaxPerTable = 3
	floorMaxPerCycle = 10
)

// floorDeps is what the floor needs of the Optimizer: the cold-start
// gate, the table context and admission.
type floorDeps interface {
	ColdStart(ctx context.Context) bool
	TableContext(ctx context.Context, snap *collector.Snapshot, table string,
	) (TableContext, bool, error)
	Admit(ctx context.Context, rec Recommendation, tc TableContext) Admission
}

// Floor is the deterministic index-candidate floor (indexes-12): when the
// tuning agent is inactive it proposes btree indexes for the statements of
// seq-scan-heavy tables, admitted only through Optimizer.Admit (the
// validator, rejection memory and the HypoPG what-if). It holds no state
// between runs; each database has its own.
type Floor struct {
	opt   floorDeps
	logFn func(string, string, ...any)
	// reh rehearses each admitted candidate on a clone (nil: no clone
	// provider, the ladder says so).
	reh IndexRehearser
}

// NewFloor is the floor of one database's optimizer; a nil optimizer
// gives a floor that runs nothing.
func NewFloor(o *Optimizer, logFn func(string, string, ...any)) *Floor {
	if o == nil {
		return &Floor{logFn: logFn}
	}
	return &Floor{opt: o, logFn: logFn}
}

// FloorResult is one floor run. Ran is false when the floor judged
// nothing (no table, cold start). Admitted are the accepted candidates;
// Incomplete are the tables whose candidates were not all judged (context
// unreadable, cycle cap reached): their open findings must not resolve.
type FloorResult struct {
	Ran        bool
	Admitted   []Admission
	Incomplete []string
}

// Run enumerates and admits the candidates of tables (quoted where
// needed, in priority order), skipping every shape in skip (the finding
// identifiers of open proposals). An admission refusal or a table whose
// context cannot be read is logged and the run goes on with the next.
func (f *Floor) Run(ctx context.Context, snap *collector.Snapshot, tables []string,
	skip map[string]bool) FloorResult {
	if f == nil || f.opt == nil || len(tables) == 0 {
		return FloorResult{}
	}
	if f.opt.ColdStart(ctx) {
		f.logFn("INFO", "optimizer: index floor: cold start, too little history to judge "+
			"the workload")
		return FloorResult{}
	}
	res := FloorResult{Ran: true}
	budget := floorMaxPerCycle
	for _, table := range tables {
		if budget == 0 {
			res.Incomplete = append(res.Incomplete, table)
			continue
		}
		admitted, used, complete := f.runTable(ctx, snap, table, skip, budget)
		res.Admitted = append(res.Admitted, admitted...)
		budget -= used
		if !complete {
			res.Incomplete = append(res.Incomplete, table)
		}
	}
	return res
}

// runTable admits up to budget candidates of one table; used counts the
// admissions it ran and complete is false when the context was unreadable
// or the budget cut its candidates.
func (f *Floor) runTable(ctx context.Context, snap *collector.Snapshot, table string,
	skip map[string]bool, budget int) (admitted []Admission, used int, complete bool) {
	tc, ok, err := f.opt.TableContext(ctx, snap, table)
	if err != nil {
		f.logFn("WARN", "optimizer: index floor: no context for %s, its candidates wait "+
			"for the next cycle: %v", table, err)
		return nil, 0, false
	}
	if !ok {
		return nil, 0, true
	}
	for _, rec := range floorCandidates(tc) {
		if skip[rec.FindingIdentifier()] {
			continue
		}
		if used == budget {
			return admitted, used, false
		}
		used++
		adm := f.opt.Admit(ctx, rec, tc)
		if adm.Outcome != AdmitAccepted {
			f.logFn("INFO", "optimizer: index floor: %s: %s %s: %s", table, rec.DDL,
				adm.Outcome, adm.Reason)
			continue
		}
		f.rehearse(ctx, &adm, tc)
		admitted = append(admitted, adm)
	}
	return admitted, used, true
}
