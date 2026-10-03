package analyzer

import (
	"context"

	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/recommendation"
	"github.com/pg-sage/sidecar/internal/selfcost"
)

// EventDispatcher sends notification events. Nil means no
// notifications are sent.
type EventDispatcher interface {
	Dispatch(ctx context.Context, event notify.Event) error
}

// ConfigAdvisor is satisfied by *advisor.Advisor without importing it.
type ConfigAdvisor interface {
	Analyze(ctx context.Context) ([]Finding, error)
}

// WorkloadForecaster produces capacity forecast findings.
type WorkloadForecaster interface {
	Forecast(ctx context.Context) ([]Finding, error)
}

// QueryTuner produces per-query tuning findings.
//
// deferredTables contains canonical "schema.table" entries that
// have pending index optimizer recommendations from the current
// or prior cycles. The tuner must skip candidates whose plans
// reference any of these tables, so a hint plan isn't installed
// just before a covering index would render it obsolete.
type QueryTuner interface {
	Tune(
		ctx context.Context,
		deferredTables map[string]bool,
	) ([]Finding, error)
}

// RCAEngine is satisfied by *rca.Engine without importing it.
type RCAEngine interface {
	Analyze(
		current *collector.Snapshot,
		previous *collector.Snapshot,
		cfg *config.Config,
		lockChainFindings []Finding,
	)
	PersistIncidents(ctx context.Context, pool *pgxpool.Pool) error
}

// SupplementalDetector adds database-backed findings without coupling the
// analyzer package to a detector implementation.
type SupplementalDetector interface {
	Detect(context.Context) ([]Finding, error)
}

// Analyzer runs the rules engine on a recurring interval, producing
// findings and persisting them to the sage.findings table.
type Analyzer struct {
	pool         *pgxpool.Pool
	cfg          *config.Config
	collector    *collector.Collector
	extras       *RuleExtras
	optimizer    *optimizer.Optimizer
	advisor      ConfigAdvisor
	forecaster   WorkloadForecaster
	tuner        QueryTuner
	rcaEngine    RCAEngine
	detectors    []SupplementalDetector
	planNarrator PlanNarrator
	logFn        func(string, string, ...any)
	dispatcher   EventDispatcher
	databaseName string
	mu           sync.RWMutex
	findings     []Finding
	// notifiedAt records the last critical notification per identity;
	// only touched by the cycle goroutine.
	notifiedAt map[string]time.Time
	// eval tracks evaluated categories for the running cycle; only
	// touched by the cycle goroutine.
	eval *cycleEval
	// lastAnalyzed and lastAnalyzedAt identify the newest snapshot a
	// cycle has already consumed; only touched by the cycle goroutine.
	lastAnalyzed   *collector.Snapshot
	lastAnalyzedAt time.Time
	// recs holds the durable recommendations each cycle proposes; nil
	// without a database. policyVersion reads the standing-policy
	// version recorded on new revisions (nil records none).
	recs          *recommendation.Store
	policyVersion func(context.Context) (int64, error)
	// cloneTracker remembers clone-family activity across cycles; only
	// touched by the cycle goroutine.
	cloneTracker *cloneTracker
	// selfCost meters pg_sage's own cost on this database (perf v1.8.3);
	// read concurrently by /metrics.
	selfCost *selfcost.Meter
}

// PlanNarrator enriches plan_regression findings with an LLM-generated
// "why did the plan change" narrative (C6). nil disables it.
type PlanNarrator interface {
	Narrate(ctx context.Context, findings []Finding) []Finding
}

// WithPlanNarrator attaches the LLM plan-regression narrator (C6).
func (a *Analyzer) WithPlanNarrator(n PlanNarrator) { a.planNarrator = n }

// New creates a new Analyzer.
func New(
	pool *pgxpool.Pool,
	cfg *config.Config,
	coll *collector.Collector,
	opt *optimizer.Optimizer,
	adv ConfigAdvisor,
	fc WorkloadForecaster,
	qt QueryTuner,
	logFn func(string, string, ...any),
) *Analyzer {
	return &Analyzer{
		pool:       pool,
		cfg:        cfg,
		collector:  coll,
		optimizer:  opt,
		advisor:    adv,
		forecaster: fc,
		tuner:      qt,
		extras: &RuleExtras{
			FirstSeen:        make(map[string]time.Time),
			RecentlyCreated:  make(map[string]time.Time),
			InvalidFirstSeen: make(map[string]time.Time),
			IndexOID:         make(map[string]uint32),
		},
		logFn:    logFn,
		recs:     newRecommendationStore(pool),
		selfCost: selfcost.NewMeter(),
	}
}

// WithDispatcher sets the notification dispatcher for critical
// finding alerts. Nil is safe (default).
func (a *Analyzer) WithDispatcher(d EventDispatcher) {
	a.dispatcher = d
}

// WithRCAEngine sets the root cause analysis engine for the analyzer.
func (a *Analyzer) WithRCAEngine(e RCAEngine) {
	a.rcaEngine = e
}

// WithSupplementalDetector attaches a detector before the analyzer starts.
func (a *Analyzer) WithSupplementalDetector(detector SupplementalDetector) {
	if detector != nil {
		a.detectors = append(a.detectors, detector)
	}
}

// WithDatabaseName sets the database name included in events.
func (a *Analyzer) WithDatabaseName(name string) {
	a.databaseName = name
}

// Run starts the analyzer loop and blocks until ctx is cancelled.
func (a *Analyzer) Run(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.Analyzer.Interval())
	defer ticker.Stop()

	a.logFn("INFO", "analyzer started, interval=%s", a.cfg.Analyzer.Interval())

	// Run once immediately.
	a.cycle(ctx)

	for {
		select {
		case <-ctx.Done():
			a.logFn("INFO", "analyzer stopped")
			return
		case <-ticker.C:
			a.cycle(ctx)
		}
	}
}

// SetFindings replaces the current findings (called by rule evaluation).
func (a *Analyzer) SetFindings(ff []Finding) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.findings = make([]Finding, len(ff))
	copy(a.findings, ff)
}

// LatestFindings returns a copy of the most recent findings under a
// read lock.
func (a *Analyzer) LatestFindings() []Finding {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]Finding, len(a.findings))
	copy(out, a.findings)
	return out
}

// Findings returns a snapshot of the current findings (alias).
func (a *Analyzer) Findings() []Finding {
	return a.LatestFindings()
}

// OpenFindingsCount returns a count of current findings by severity.
func (a *Analyzer) OpenFindingsCount() map[string]int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	counts := make(map[string]int)
	for _, f := range a.findings {
		counts[f.Severity]++
	}
	return counts
}

// filterSchemaExclusions removes sage/pg_catalog/information_schema
// objects from snapshot data before rules run.
func filterSchemaExclusions(snap *collector.Snapshot) {
	excluded := map[string]bool{
		"sage": true, "pgsnap": true,
		"pg_catalog": true, "information_schema": true,
		"google_ml": true,
	}

	filtered := snap.Tables[:0]
	for _, t := range snap.Tables {
		if !excluded[t.SchemaName] {
			filtered = append(filtered, t)
		}
	}
	snap.Tables = filtered

	idxFiltered := snap.Indexes[:0]
	for _, idx := range snap.Indexes {
		if !excluded[idx.SchemaName] {
			idxFiltered = append(idxFiltered, idx)
		}
	}
	snap.Indexes = idxFiltered
}

func snapshotForAnalysis(source *collector.Snapshot) *collector.Snapshot {
	if source == nil {
		return nil
	}
	private := *source
	private.Tables = append([]collector.TableStats(nil), source.Tables...)
	private.Indexes = append([]collector.IndexStats(nil), source.Indexes...)
	return &private
}
