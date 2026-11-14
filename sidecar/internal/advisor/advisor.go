package advisor

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// Advisor orchestrates all configuration advisory features.
type Advisor struct {
	cfg    *config.Config
	llmMgr *llm.Manager
	logFn  func(string, string, ...any)

	// Per-instance overrides for fleet mode. When empty, falls
	// back to cfg.CloudEnvironment / cfg.Postgres.Database.
	cloudEnv string
	dbName   string
	// pgMajor is the reviewed server's major version (0: unknown).
	pgMajor int

	// hostMemoryBytes is operator-supplied host RAM. PostgreSQL exposes
	// no RAM figure; without it shared_buffers changes stay advisory.
	hostMemoryBytes int64
	// hostMemorySource is live host memory (managed-cloud telemetry).
	hostMemorySource HostMemorySource

	// facts renders the confirmed facts for prompts (roadmap 2.3); nil: none.
	facts FactSource
	// instanceOwner says whether this database owns its instance's
	// settings in fleet mode (advisors-08); nil: it does.
	instanceOwner InstanceOwner

	snaps    snapshotSource
	connHist connHistorySource // nil: the connection history is unknown
	hist     windowHistory     // nil: the WAL and bloat window is unknown
	open     openFindingStore
	runs     runStore
	schedule runSchedule
	streaks  clearStreaks
	now      func() time.Time

	mu        sync.Mutex
	findings  []analyzer.Finding
	evaluated []string
}

// snapshotSource is the collector's view the advisor reviews. The window
// it compares with is read from the history store (advisors-05), not the
// collector's previous snapshot.
type snapshotSource interface {
	LatestSnapshot() *collector.Snapshot
}

func New(
	pool *pgxpool.Pool,
	cfg *config.Config,
	coll *collector.Collector,
	llmMgr *llm.Manager,
	logFn func(string, string, ...any),
) *Advisor {
	a := &Advisor{
		cfg:    cfg,
		llmMgr: llmMgr,
		logFn:  logFn,
		open:   noOpenFindings{},
		runs:   newMemRunStore(),
		now:    time.Now,
	}
	if coll != nil {
		a.snaps = coll
	}
	if pool != nil {
		a.open, a.runs = pgOpenFindings{pool}, pgRunStore{pool}
		a.connHist = pgConnHistory{db: pool}
		a.hist = pgWindowHistory{db: pool}
	}
	return a
}

// WithCloudEnv sets the cloud environment for this advisor instance.
// Use in fleet mode where each database may be on a different platform.
func (a *Advisor) WithCloudEnv(env string) {
	a.cloudEnv = env
}

// WithHostMemoryBytes supplies host RAM so shared_buffers
// recommendations can be grounded (G3-B08). Zero keeps them advisory.
func (a *Advisor) WithHostMemoryBytes(n int64) {
	a.hostMemoryBytes = n
}

// WithDatabaseName sets the target database name for this advisor.
// Used to generate ALTER DATABASE statements on managed services.
func (a *Advisor) WithDatabaseName(name string) {
	a.dbName = name
}

// subAdvisor is one LLM sub-advisor and the category it owns. Vacuum and
// memory tuning belong to the tuning agent (roadmap 2.2); the advisor
// keeps instance capacity and maintenance advice.
type subAdvisor struct {
	name, category string
	enabled        bool
	run            func(context.Context) ([]analyzer.Finding, error)
}

func (a *Advisor) subAdvisors(snap *collector.Snapshot) []subAdvisor {
	c := a.cfg.Advisor
	return []subAdvisor{
		{"wal", "wal_tuning", c.WALEnabled, func(ctx context.Context) (
			[]analyzer.Finding, error) {
			return analyzeWAL(a.windowContext(ctx), a.llmMgr, snap, a.cfg, a.logFn)
		}},
		{"connections", "connection_tuning", c.ConnectionEnabled, func(ctx context.Context) (
			[]analyzer.Finding, error) {
			return analyzeConnections(a.connectionContext(ctx), a.llmMgr, snap, a.cfg,
				a.logFn)
		}},
		{"rewrites", "query_rewrite", c.RewriteEnabled, func(ctx context.Context) (
			[]analyzer.Finding, error) {
			return analyzeQueryRewrites(ctx, a.llmMgr, snap, a.cfg, a.logFn)
		}},
		{"bloat", "bloat_remediation", c.BloatEnabled, func(ctx context.Context) (
			[]analyzer.Finding, error) {
			return analyzeBloat(a.windowContext(ctx), a.llmMgr, snap, a.cfg, a.logFn)
		}},
	}
}

// Analyze re-checks the open advisor findings and runs the due
// sub-advisors on the latest snapshot.
func (a *Advisor) Analyze(ctx context.Context) ([]analyzer.Finding, error) {
	if !a.cfg.Advisor.Enabled {
		// Disabled on purpose: the degraded warning no longer applies.
		a.setEvaluated([]string{"advisor_degraded"})
		return nil, nil
	}
	var snap *collector.Snapshot
	if a.snaps != nil {
		snap = a.snaps.LatestSnapshot()
	}
	return a.analyzeSnapshot(ctx, snap), nil
}

// analyzeSnapshot is one advisor cycle on snap. The deterministic re-check
// runs with the LLM on or off; only the sub-advisors need the model.
func (a *Advisor) analyzeSnapshot(ctx context.Context, snap *collector.Snapshot,
) []analyzer.Finding {
	var out []analyzer.Finding
	var evaluated []string
	llmOn := a.cfg.LLM.Enabled
	if llmOn {
		evaluated = append(evaluated, "advisor_degraded")
	} else {
		out = append(out, advisorDegradedFinding())
	}
	if snap == nil || snap.ConfigData == nil {
		a.logFn("DEBUG", "advisor: no config snapshot yet")
		return a.finish(out, evaluated)
	}
	own := a.ownership()
	if io, ok := a.ioSettingsCycle(snap, own); ok {
		out, evaluated = append(out, io...), append(evaluated, ioSettingsCategory)
	}
	open, err := a.open.OpenFindings(ctx, advisorCategories)
	if err != nil {
		// Fail closed on spend: without the open set the gate is unknown.
		a.logFn("WARN", "advisor: open findings unreadable, sub-advisors skipped "+
			"this cycle: %v", err)
		return a.finish(out, evaluated)
	}
	checked, aside := splitInstanceOpen(open, own)
	res := a.resolveOpen(snap, checked)
	out = append(out, res.kept...)
	var r subRun
	if llmOn {
		r = a.review(withOwnership(withOpenGate(ctx, res.gate), own), snap)
		for c := range r.failed {
			res.evaluated[c] = false
		}
	}
	out = append(append(out, keepAside(aside, r, own)...), r.findings...)
	for _, c := range advisorCategories {
		// A non-owner leaves its open instance rows to the owner: they are
		// neither re-checked nor resolved here.
		if res.evaluated[c] && !(own.defers() && instanceCategories[c]) {
			evaluated = append(evaluated, c)
		}
	}
	return a.finish(out, evaluated)
}

// finish publishes one cycle's findings and evaluated categories.
func (a *Advisor) finish(out []analyzer.Finding, evaluated []string) []analyzer.Finding {
	a.setEvaluated(evaluated)
	a.mu.Lock()
	a.findings = out
	a.mu.Unlock()
	a.logFn("INFO", "advisor: %d findings (%d categories re-checked)", len(out),
		len(evaluated))
	return out
}

func (a *Advisor) setEvaluated(categories []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.evaluated = categories
}

// LastEvaluatedCategories are the categories the latest cycle fully
// re-checked (analyzer.EvaluatedCategoryReporter): their open findings
// that the cycle did not re-emit resolve. A category whose evidence was
// missing, or whose sub-advisor failed, is not among them.
func (a *Advisor) LastEvaluatedCategories() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.evaluated...)
}

// subRun is what the due sub-advisors produced in one cycle.
type subRun struct {
	findings     []analyzer.Finding
	budgetErrors int
	failed       map[string]bool // categories whose sub-advisor failed
	ran          map[string]bool // categories whose sub-advisor ran and answered
}

// runSubAdvisors runs each enabled sub-advisor that is due and claimed.
// A failure (model error, token budget) backs the sub-advisor off and
// leaves its category unevaluated; new findings about an object that is
// already open are dropped.
func (a *Advisor) runSubAdvisors(ctx context.Context, snap *collector.Snapshot,
) subRun {
	r := subRun{failed: map[string]bool{}, ran: map[string]bool{}}
	gate := openGateFrom(ctx)
	now := a.now()
	a.loadSchedule(ctx)
	own := ownershipFrom(ctx)
	for _, sa := range a.subAdvisors(snap) {
		if own.defers() && instanceCategories[sa.category] {
			a.logFn("DEBUG", "advisor: %s: instance settings are managed from "+
				"database %s", sa.name, own.owner)
			continue
		}
		if !sa.enabled || !a.claim(ctx, sa.name, now) {
			continue
		}
		findings, err := sa.run(ctx)
		a.record(ctx, sa.name, now, err)
		if err != nil {
			a.logFn("WARN", "advisor: %s: %v", sa.name, err)
			r.failed[sa.category] = true
			if isBudgetError(err) {
				r.budgetErrors++
			}
			continue
		}
		r.ran[sa.category] = true
		r.findings = append(r.findings, gate.dropOpen(sa.category, findings, a.logFn)...)
	}
	if r.budgetErrors > 0 {
		a.logFn("WARN", "advisor: %d sub-advisors skipped: daily token budget exhausted",
			r.budgetErrors)
	}
	return r
}

func advisorDegradedFinding() analyzer.Finding {
	return analyzer.Finding{
		Category:         "advisor_degraded",
		Severity:         "warning",
		ObjectType:       "advisor",
		ObjectIdentifier: "llm",
		Title:            "Configuration advisor is enabled but LLM is disabled",
		Detail: map[string]any{
			"advisor_enabled": true,
			"llm_enabled":     false,
			"mode":            "degraded",
		},
		Recommendation: "Enable and configure the LLM provider, or disable " +
			"advisor features intentionally so this is not mistaken for a " +
			"healthy no-finding cycle.",
	}
}

// isBudgetError returns true if the error indicates
// the daily token budget has been exhausted.
func isBudgetError(err error) bool {
	return err != nil &&
		strings.Contains(err.Error(), "budget exhausted")
}

func (a *Advisor) LatestFindings() []analyzer.Finding {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]analyzer.Finding, len(a.findings))
	copy(out, a.findings)
	return out
}
