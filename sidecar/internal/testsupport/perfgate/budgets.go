package perfgate

import "fmt"

// Budgets is the one place the gate's limits live. Each one encodes the
// product requirement that a DBA never has to add an index, a hint, a
// setting or a retention job to make pg_sage itself behave on a large,
// messy database (reviews/2026-10-03-perf-gate-report.md).
type Budgets struct {
	// SeqScanMinRows (gate A): no sequential scan of a sage table holding
	// more rows than this. Small tables (config, policy, bindings) are
	// cheaper to scan than to index; history tables are not.
	SeqScanMinRows int64
	// StatementMeanMs (gate B): the mean execution time of every pg_sage
	// statement in the steady phase.
	StatementMeanMs float64
	// CycleDBTimeMs (gate B): pg_sage's total execution time per collector
	// cycle with every component running once per cycle. 3 s is 5% of one
	// core at the default 60 s collector interval.
	CycleDBTimeMs float64
	// RowsWrittenPerCycle (gate C): rows inserted, updated or deleted per
	// sage table per cycle in steady state. A writer that is O(changes)
	// stays far below it; one that is O(objects) writes thousands.
	RowsWrittenPerCycle int64
	// CatalogStatementMaxMs (gate D): the slowest single execution of any
	// catalog statement, the incident budget the collector pages under.
	// A statement cancelled by a timeout fails this gate too.
	CatalogStatementMaxMs float64
	// EndpointMaxMs (gate E): one API list endpoint call, and it must
	// answer 200.
	EndpointMaxMs float64
	// HotUpdateMinPct and HotMinUpdates (gate F): a sage table updated at
	// least HotMinUpdates times in the steady phase writes at least
	// HotUpdateMinPct percent of them as heap-only (HOT) updates: its
	// updated columns are not indexed and its pages keep room for them.
	HotUpdateMinPct float64
	HotMinUpdates   int64
	// SidecarCPUMsPerCycle (gate G): the sidecar process's CPU time per
	// collector cycle in the steady phase, every component running once
	// per (15 s) cycle. 310 ms measured at small scale (2026-10-04); 600
	// leaves room for a loaded host, not for a regression that doubles it.
	SidecarCPUMsPerCycle float64
}

// DefaultBudgets are the shipped limits.
func DefaultBudgets() Budgets {
	return Budgets{
		SeqScanMinRows:        5000,
		StatementMeanMs:       100,
		CycleDBTimeMs:         3000,
		RowsWrittenPerCycle:   250,
		CatalogStatementMaxMs: 500,
		EndpointMaxMs:         1000,
		HotUpdateMinPct:       50,
		HotMinUpdates:         5,
		SidecarCPUMsPerCycle:  600,
	}
}

func (b Budgets) validate() error {
	if b.SeqScanMinRows <= 0 || b.StatementMeanMs <= 0 || b.CycleDBTimeMs <= 0 ||
		b.RowsWrittenPerCycle <= 0 || b.CatalogStatementMaxMs <= 0 || b.EndpointMaxMs <= 0 ||
		b.HotUpdateMinPct <= 0 || b.HotMinUpdates <= 0 || b.SidecarCPUMsPerCycle <= 0 {
		return fmt.Errorf("perfgate: every budget must be positive: %+v", b)
	}
	return nil
}
