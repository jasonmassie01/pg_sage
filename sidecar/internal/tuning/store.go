package tuning

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/tuner"
)

// The agent's dependencies. Each is an interface so the agent runs on
// fakes in unit tests; the production implementations are the optimizer,
// the facts store, the tuner and the PostgreSQL store below.

// IndexTools are the optimizer's index tools: table context, admission
// (validator, rejection memory, HypoPG what-if) and memory.
type IndexTools interface {
	ColdStart(ctx context.Context) bool
	TableContext(ctx context.Context, snap *collector.Snapshot, table string) (
		optimizer.TableContext, bool, error)
	Admit(ctx context.Context, rec optimizer.Recommendation,
		tc optimizer.TableContext) optimizer.Admission
	MeasuredRejections(ctx context.Context, tc optimizer.TableContext) []string
	MemoryStats() optimizer.MemoryStats
}

// FactSource lists facts (facts.Store).
type FactSource interface {
	List(ctx context.Context, f facts.Filter) ([]facts.Fact, error)
}

// HintSink turns a hint into a finding with the tuner's safeguards
// (CheckHint, no side effects) and records the hints the agent keeps.
type HintSink interface {
	HintsAvailable() bool
	CheckHint(ctx context.Context, p tuner.HintProposal) (analyzer.Finding, error)
	RecordHint(ctx context.Context, p tuner.HintProposal) error
}

// Store is what the agent reads from the sage schema and the catalog.
type Store interface {
	// OpenFindings are the open findings of the categories.
	OpenFindings(ctx context.Context, categories []string) ([]analyzer.Finding, error)
	// OperatorRejected are the normalized SQL of queue items an operator
	// rejected since then.
	OperatorRejected(ctx context.Context, since time.Time) (map[string]bool, error)
	// Outcomes are the decided outcomes of the classes since then, newest
	// first, at most limit per class.
	Outcomes(ctx context.Context, classes []string, since time.Time, limit int) (
		[]OutcomeSample, error)
	// Plan is a statement's plan: the explain cache first, else a plan of
	// its own (EXPLAIN, never ANALYZE).
	Plan(ctx context.Context, queryID int64, text string) (Plan, error)
	// ExtendedStats are the extended statistics objects on a table.
	ExtendedStats(ctx context.Context, schema, table string) ([]ExtStat, error)
	// ColumnStats are pg_stats of the named columns, in request order.
	ColumnStats(ctx context.Context, schema, table string, cols []string) (
		[]ColumnStat, error)
	// Relations reports which of the tables and indexes exist and the valid
	// index definitions of the existing tables, in one catalog read.
	Relations(ctx context.Context, tables, indexes []string) (CatalogState, error)
	// SettingActions are the setting and storage-parameter changes executed
	// since then, oldest first, with their verification verdicts.
	SettingActions(ctx context.Context, since time.Time) ([]SettingAction, error)
	// DayBudgetUsed is the model spend charged on the UTC day of day.
	DayBudgetUsed(ctx context.Context, day time.Time) (tokens, requests int64, err error)
	// ChargeDayBudget adds spend to the UTC day of day; it survives
	// restarts.
	ChargeDayBudget(ctx context.Context, day time.Time, tokens, requests int64) error
}

// CatalogState is what Relations found, keyed by the names asked. A name
// that is not a qualified identifier is absent: unknown, never "gone".
type CatalogState struct {
	Tables    map[string]bool
	Indexes   map[string]bool
	IndexDefs map[string][]string
}

// Plan sources.
const (
	PlanSourceCache   = "explain_cache"
	PlanSourceGeneric = "generic_plan"
	PlanSourceExplain = "explain"
	PlanSourceNone    = "none"
)

// Plan is a statement's plan as JSON.
type Plan struct {
	Source string
	JSON   []byte
}

// ExtStat is one extended statistics object.
type ExtStat struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Kinds   []string `json:"kinds"`
}

// ColumnStat is one column's planner statistics.
type ColumnStat struct {
	Column      string  `json:"column"`
	NDistinct   float64 `json:"n_distinct"`
	Correlation float64 `json:"correlation"`
	NullFrac    float64 `json:"null_frac"`
	AvgWidth    int     `json:"avg_width"`
}
