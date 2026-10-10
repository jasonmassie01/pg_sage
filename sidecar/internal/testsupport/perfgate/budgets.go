package perfgate

import (
	"fmt"
	"strings"
)

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
	// MeanExempt names, by the tag in their comment, the statements the
	// mean budget judges against their own ceiling instead, each with why
	// no index, hint or setting can bring it under StatementMeanMs. Above
	// its ceiling a statement is a gate B offender. They still count
	// toward CycleDBTimeMs and gate D. It is reported with the budgets.
	MeanExempt map[string]MeanExemption
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
	// HotExempt names the sage tables one of whose update statements
	// cannot be HOT by design (it moves a row between the partial indexes
	// it is read through, changing an indexed column on purpose): gate F
	// subtracts the rows of the statement carrying the exemption's tag
	// from the table's updates and charges the rest. It is reported with
	// the budgets.
	HotExempt map[string]HotExemption
	// SidecarCPUMsPerCycle (gate G): the sidecar process's CPU time per
	// collector cycle in the steady phase, every component running once
	// per (15 s) cycle. 310 ms measured at small scale (2026-10-04); 600
	// leaves room for a loaded host, not for a regression that doubles it.
	SidecarCPUMsPerCycle float64
}

// MeanExemption is why a tagged statement cannot meet the mean budget,
// and the mean it must still stay under.
type MeanExemption struct {
	Reason    string
	CeilingMs float64
}

// HotExemption is why one update statement of a table cannot be HOT; Tag
// is that statement's tag (/* pg_sage <tag> v1 */).
type HotExemption struct {
	Reason string
	Tag    string
}

// DefaultBudgets are the shipped limits.
func DefaultBudgets() Budgets {
	return Budgets{
		SeqScanMinRows:        5000,
		StatementMeanMs:       100,
		MeanExempt:            defaultMeanExempt(),
		CycleDBTimeMs:         3000,
		RowsWrittenPerCycle:   250,
		CatalogStatementMaxMs: 500,
		EndpointMaxMs:         1000,
		HotUpdateMinPct:       50,
		HotMinUpdates:         5,
		HotExempt: map[string]HotExemption{
			"sage.shadow_decision": {Tag: "shadow:score", Reason: "the score write " +
				"sets status, score, counted and scored_at once per decision, moving it " +
				"from the partial pending index to the partial scored index and changing " +
				"the class-summary index's INCLUDE columns, so it cannot be HOT and its " +
				"rows are not charged; the seen bump, the insert's ON CONFLICT bump and " +
				"markApplied set only unindexed columns and are charged"},
		},
		SidecarCPUMsPerCycle: 600,
	}
}

// defaultMeanExempt: each ceiling sits just above the slowest mean seen
// on CI (250 ms, 135 ms), so a regression in either still fails gate B.
func defaultMeanExempt() map[string]MeanExemption {
	return map[string]MeanExemption{
		"sre:cluster_database_size": {CeilingMs: 300, Reason: "pg_database_size stats " +
			"every file of every database: its time follows the cluster's file count and " +
			"the disk, not the SQL (105-250 ms on CI, 37 ms locally on the same fixture); " +
			"no index, hint or setting makes it faster"},
		"schema_guard:structural": {CeilingMs: 200, Reason: "the schema guard's " +
			"structural scan reads every column of every user table once (92-135 ms on " +
			"CI at 20,000 relations); it reruns only when the catalog counters move, at " +
			"most every 5 min, not every cycle"},
	}
}

func (b Budgets) validate() error {
	if b.SeqScanMinRows <= 0 || b.StatementMeanMs <= 0 || b.CycleDBTimeMs <= 0 ||
		b.RowsWrittenPerCycle <= 0 || b.CatalogStatementMaxMs <= 0 || b.EndpointMaxMs <= 0 ||
		b.HotUpdateMinPct <= 0 || b.HotMinUpdates <= 0 || b.SidecarCPUMsPerCycle <= 0 {
		return fmt.Errorf("perfgate: every budget must be positive: %+v", b)
	}
	for tag, ex := range b.MeanExempt {
		if strings.TrimSpace(tag) == "" || strings.TrimSpace(ex.Reason) == "" ||
			!(ex.CeilingMs > b.StatementMeanMs) {
			return fmt.Errorf("perfgate: mean exemption %q needs a tag, a reason and a "+
				"ceiling above the %.0f ms budget: %+v", tag, b.StatementMeanMs, ex)
		}
	}
	for table, ex := range b.HotExempt {
		if strings.TrimSpace(table) == "" || strings.TrimSpace(ex.Tag) == "" ||
			strings.TrimSpace(ex.Reason) == "" {
			return fmt.Errorf("perfgate: HOT exemption of %q needs a statement tag and "+
				"a reason: %+v", table, ex)
		}
	}
	return nil
}
