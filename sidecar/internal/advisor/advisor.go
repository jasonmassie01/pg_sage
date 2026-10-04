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
	pool   *pgxpool.Pool
	cfg    *config.Config
	coll   *collector.Collector
	llmMgr *llm.Manager
	logFn  func(string, string, ...any)

	// Per-instance overrides for fleet mode. When empty, falls
	// back to cfg.CloudEnvironment / cfg.Postgres.Database.
	cloudEnv string
	dbName   string

	// hostMemoryBytes is operator-supplied host RAM. PostgreSQL exposes
	// no RAM figure; without it shared_buffers changes stay advisory.
	hostMemoryBytes int64

	// facts renders the confirmed facts for prompts (roadmap 2.3); nil: none.
	facts FactSource

	mu        sync.Mutex
	lastRunAt time.Time
	findings  []analyzer.Finding
}

func New(
	pool *pgxpool.Pool,
	cfg *config.Config,
	coll *collector.Collector,
	llmMgr *llm.Manager,
	logFn func(string, string, ...any),
) *Advisor {
	return &Advisor{
		pool:   pool,
		cfg:    cfg,
		coll:   coll,
		llmMgr: llmMgr,
		logFn:  logFn,
	}
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

func (a *Advisor) ShouldRun() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return time.Since(a.lastRunAt) > a.cfg.Advisor.Interval()
}

// subAdvisor is one LLM sub-advisor and the category it owns. Vacuum and
// memory tuning belong to the tuning agent (roadmap 2.2); the advisor
// keeps instance capacity and maintenance advice.
type subAdvisor struct {
	name, category string
	enabled        bool
	run            func(context.Context) ([]analyzer.Finding, error)
}

func (a *Advisor) subAdvisors(snap, prev *collector.Snapshot) []subAdvisor {
	c := a.cfg.Advisor
	return []subAdvisor{
		{"wal", "wal_tuning", c.WALEnabled, func(ctx context.Context) (
			[]analyzer.Finding, error) {
			return analyzeWAL(ctx, a.llmMgr, snap, prev, a.cfg, a.logFn)
		}},
		{"connections", "connection_tuning", c.ConnectionEnabled, func(ctx context.Context) (
			[]analyzer.Finding, error) {
			return analyzeConnections(ctx, a.llmMgr, snap, a.cfg, a.logFn)
		}},
		{"rewrites", "query_rewrite", c.RewriteEnabled, func(ctx context.Context) (
			[]analyzer.Finding, error) {
			return analyzeQueryRewrites(ctx, a.pool, a.llmMgr, snap, a.cfg, a.logFn)
		}},
		{"bloat", "bloat_remediation", c.BloatEnabled, func(ctx context.Context) (
			[]analyzer.Finding, error) {
			return analyzeBloat(ctx, a.llmMgr, snap, prev, a.cfg, a.logFn)
		}},
	}
}

// runSubAdvisors runs the enabled sub-advisors whose category has no open
// finding; it returns their findings and how many hit the token budget.
func (a *Advisor) runSubAdvisors(ctx context.Context, snap, prev *collector.Snapshot) (
	[]analyzer.Finding, int) {
	var all []analyzer.Finding
	budgetErrors := 0
	for _, sa := range a.subAdvisors(snap, prev) {
		if !sa.enabled {
			continue
		}
		if a.hasOpenFindings(ctx, sa.category) {
			a.logFn("DEBUG", "advisor: %s: skipping, open findings exist", sa.name)
			continue
		}
		findings, err := sa.run(ctx)
		if err != nil {
			a.logFn("WARN", "advisor: %s: %v", sa.name, err)
			if isBudgetError(err) {
				budgetErrors++
			}
			continue
		}
		all = append(all, findings...)
	}
	return all, budgetErrors
}

// Analyze runs all enabled sub-advisors and returns findings.
func (a *Advisor) Analyze(ctx context.Context) ([]analyzer.Finding, error) {
	if !a.cfg.Advisor.Enabled {
		return nil, nil
	}
	if !a.cfg.LLM.Enabled {
		return []analyzer.Finding{advisorDegradedFinding()}, nil
	}
	if !a.ShouldRun() {
		return nil, nil
	}

	a.logFn("INFO", "advisor: starting configuration review")
	ctx = a.factsContext(ctx)

	snap := a.coll.LatestSnapshot()
	prev := a.coll.PreviousSnapshot()
	if snap == nil || snap.ConfigData == nil {
		a.logFn("DEBUG", "advisor: no config snapshot yet")
		return nil, nil
	}

	all, budgetErrors := a.runSubAdvisors(ctx, snap, prev)
	cloudEnv := a.cloudEnv
	if cloudEnv == "" {
		cloudEnv = a.cfg.CloudEnvironment
	}
	dbName := a.dbName
	if dbName == "" {
		dbName = a.cfg.Postgres.Database
	}
	all = GateConfigFindings(all, a.hostMemoryBytes, cloudEnv, dbName,
		snap.ConfigData.PGSettings)

	a.mu.Lock()
	a.lastRunAt = time.Now()
	a.findings = all
	a.mu.Unlock()

	if budgetErrors > 0 {
		a.logFn("WARN",
			"advisor: produced %d findings "+
				"(%d sub-advisors skipped: daily token budget exhausted)",
			len(all), budgetErrors)
	} else {
		a.logFn("INFO", "advisor: produced %d findings", len(all))
	}
	return all, nil
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

// hasOpenFindings returns true if sage.findings already has open
// findings for the given category, avoiding redundant LLM calls.
func (a *Advisor) hasOpenFindings(
	ctx context.Context, category string,
) bool {
	if a.pool == nil {
		return false
	}
	var count int
	err := a.pool.QueryRow(ctx,
		`/* pg_sage */ SELECT count(*) FROM sage.findings
		 WHERE category = $1
		   AND status = 'open'
		   AND acted_on_at IS NULL`,
		category,
	).Scan(&count)
	if err != nil {
		return false
	}
	return count > 0
}
