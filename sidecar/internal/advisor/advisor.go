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
		findings, err := analyzeVacuum(ctx, a.llmMgr, snap, prev, a.cfg, a.logFn)
		if err != nil {
			a.logFn("WARN", "advisor: vacuum: %v", err)
		} else {
			all = append(all, findings...)
		}
	}

	if a.cfg.Advisor.WALEnabled {
		findings, err := analyzeWAL(ctx, a.llmMgr, snap, prev, a.cfg, a.logFn)
		if err != nil {
			a.logFn("WARN", "advisor: wal: %v", err)
		} else {
			all = append(all, findings...)
		}
	}

	if a.cfg.Advisor.ConnectionEnabled {
		findings, err := analyzeConnections(ctx, a.llmMgr, snap, a.cfg, a.logFn)
		if err != nil {
			a.logFn("WARN", "advisor: connections: %v", err)
		} else {
			all = append(all, findings...)
		}
	}

	// Group 2: Workload intelligence
	if a.cfg.Advisor.MemoryEnabled {
		findings, err := analyzeMemory(ctx, a.llmMgr, snap, a.cfg, a.logFn)
		if err != nil {
			a.logFn("WARN", "advisor: memory: %v", err)
		} else {
			all = append(all, findings...)
		}
	}

	if a.cfg.Advisor.RewriteEnabled {
		findings, err := analyzeQueryRewrites(ctx, a.llmMgr, snap, a.cfg, a.logFn)
		if err != nil {
			a.logFn("WARN", "advisor: rewrites: %v", err)
		} else {
			all = append(all, findings...)
		}
	}

	if a.cfg.Advisor.BloatEnabled {
		findings, err := analyzeBloat(ctx, a.llmMgr, snap, prev, a.cfg, a.logFn)
		if err != nil {
			a.logFn("WARN", "advisor: bloat: %v", err)
		} else {
			all = append(all, findings...)
		}
	}

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
