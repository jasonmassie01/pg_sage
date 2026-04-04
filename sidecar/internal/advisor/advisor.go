package advisor

import (
	"context"
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

// Analyze runs all enabled sub-advisors and returns findings.
func (a *Advisor) Analyze(ctx context.Context) ([]analyzer.Finding, error) {
	if !a.cfg.Advisor.Enabled || !a.cfg.LLM.Enabled {
		return nil, nil
	}
	if !a.ShouldRun() {
		return nil, nil
	}

	a.logFn("INFO", "advisor: starting configuration review")

	snap := a.coll.LatestSnapshot()
	prev := a.coll.PreviousSnapshot()
	if snap == nil || snap.ConfigData == nil {
		a.logFn("DEBUG", "advisor: no config snapshot yet")
		return nil, nil
	}

	var all []analyzer.Finding

	// Group 1: Configuration tuning
	if a.cfg.Advisor.VacuumEnabled {
		if a.hasOpenFindings(ctx, "vacuum_tuning") {
			a.logFn("DEBUG", "advisor: vacuum: skipping, open findings exist")
		} else {
			findings, err := analyzeVacuum(ctx, a.llmMgr, snap, prev, a.cfg, a.logFn)
			if err != nil {
				a.logFn("WARN", "advisor: vacuum: %v", err)
			} else {
				all = append(all, findings...)
			}
		}
	}

	if a.cfg.Advisor.WALEnabled {
		if a.hasOpenFindings(ctx, "wal_tuning") {
			a.logFn("DEBUG", "advisor: wal: skipping, open findings exist")
		} else {
			findings, err := analyzeWAL(ctx, a.llmMgr, snap, prev, a.cfg, a.logFn)
			if err != nil {
				a.logFn("WARN", "advisor: wal: %v", err)
			} else {
				all = append(all, findings...)
			}
		}
	}

	if a.cfg.Advisor.ConnectionEnabled {
		if a.hasOpenFindings(ctx, "connection_tuning") {
			a.logFn("DEBUG",
				"advisor: connections: skipping, open findings exist")
		} else {
			findings, err := analyzeConnections(ctx, a.llmMgr, snap, a.cfg, a.logFn)
			if err != nil {
				a.logFn("WARN", "advisor: connections: %v", err)
			} else {
				all = append(all, findings...)
			}
		}
	}

	// Group 2: Workload intelligence
	if a.cfg.Advisor.MemoryEnabled {
		if a.hasOpenFindings(ctx, "memory_tuning") {
			a.logFn("DEBUG", "advisor: memory: skipping, open findings exist")
		} else {
			findings, err := analyzeMemory(ctx, a.llmMgr, snap, a.cfg, a.logFn)
			if err != nil {
				a.logFn("WARN", "advisor: memory: %v", err)
			} else {
				all = append(all, findings...)
			}
		}
	}

	if a.cfg.Advisor.RewriteEnabled {
		if a.hasOpenFindings(ctx, "query_rewrite") {
			a.logFn("DEBUG",
				"advisor: rewrites: skipping, open findings exist")
		} else {
			findings, err := analyzeQueryRewrites(ctx, a.llmMgr, snap, a.cfg, a.logFn)
			if err != nil {
				a.logFn("WARN", "advisor: rewrites: %v", err)
			} else {
				all = append(all, findings...)
			}
		}
	}

	if a.cfg.Advisor.BloatEnabled {
		if a.hasOpenFindings(ctx, "bloat_analysis") {
			a.logFn("DEBUG", "advisor: bloat: skipping, open findings exist")
		} else {
			findings, err := analyzeBloat(ctx, a.llmMgr, snap, prev, a.cfg, a.logFn)
			if err != nil {
				a.logFn("WARN", "advisor: bloat: %v", err)
			} else {
				all = append(all, findings...)
			}
		}
	}

	// Rewrite findings for cloud platforms (ALTER SYSTEM →
	// ALTER DATABASE, filter restart-requiring GUCs).
	cloudEnv := a.cloudEnv
	if cloudEnv == "" {
		cloudEnv = a.cfg.CloudEnvironment
	}
	dbName := a.dbName
	if dbName == "" {
		dbName = a.cfg.Postgres.Database
	}
	all = TransformForCloud(all, cloudEnv, dbName)

	a.mu.Lock()
	a.lastRunAt = time.Now()
	a.findings = all
	a.mu.Unlock()

	a.logFn("INFO", "advisor: produced %d findings", len(all))
	return all, nil
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
		`SELECT count(*) FROM sage.findings
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
