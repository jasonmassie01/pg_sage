package upgradecheck

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/pgversion"
)

// monitorInterval is how often the case is re-checked on a member that is
// due for an upgrade.
const monitorInterval = 24 * time.Hour

// MonitorOptions configure a Monitor.
type MonitorOptions struct {
	// Provider is the live cloud_environment; nil is "".
	Provider func() string
	// TargetMajor is the major the daily case plans for; 0 is the newest
	// pg_sage knows.
	TargetMajor int
	// Due reports whether a member's major is due for an upgrade; nil is
	// pgversion.NearEOL (where AP-10 reports end of life).
	Due func(major int, now time.Time) bool
	// Now is the clock; nil is time.Now.
	Now              func() time.Time
	StatementTimeout time.Duration // 0: DefaultStatementTimeout
}

// Monitor runs the readiness case in the analyzer (an
// analyzer.SupplementalDetector): once a day, and only for a member whose
// major is at or near end of life. Between runs it reports nothing and
// evaluates nothing, so open findings stay; a completed run (also one
// that finds the member no longer due) evaluates the category, so a fixed
// check's finding resolves.
type Monitor struct {
	pool      *pgxpool.Pool
	opts      MonitorOptions
	mu        sync.Mutex
	lastRun   time.Time
	evaluated []string
}

var _ analyzer.EvaluatedCategoryReporter = (*Monitor)(nil)

// NewMonitor returns a monitor of pool's database.
func NewMonitor(pool *pgxpool.Pool, opts MonitorOptions) *Monitor {
	if opts.Provider == nil {
		opts.Provider = func() string { return "" }
	}
	if opts.TargetMajor == 0 {
		opts.TargetMajor = pgversion.NewestKnownMajor()
	}
	if opts.Due == nil {
		opts.Due = pgversion.NearEOL
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Monitor{pool: pool, opts: opts}
}

var versionSQL = Tag + `SELECT current_setting('server_version_num')::int`

// Detect runs the case when it is due and returns its findings.
func (m *Monitor) Detect(ctx context.Context) ([]analyzer.Finding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.evaluated = nil
	if m.pool == nil {
		return nil, ErrNoPool
	}
	now := m.opts.Now()
	if !m.lastRun.IsZero() && now.Sub(m.lastRun) < monitorInterval {
		return nil, nil
	}
	timeout := m.opts.StatementTimeout
	if timeout <= 0 {
		timeout = DefaultStatementTimeout
	}
	var versionNum int
	reader := catalogread.New(m.pool, catalogread.Timeouts{Statement: timeout,
		Lock: time.Second})
	if err := reader.QueryRow(ctx, versionSQL).Scan(&versionNum); err != nil {
		return nil, fmt.Errorf("upgrade readiness: read the server version: %w", err)
	}
	major := versionNum / 10000
	if !m.opts.Due(major, now) || major >= m.opts.TargetMajor {
		m.lastRun, m.evaluated = now, []string{Category}
		return nil, nil
	}
	rep, err := Run(ctx, m.pool, Target{Major: m.opts.TargetMajor, Method: PGUpgrade,
		Provider: m.opts.Provider()}, Options{StatementTimeout: timeout,
		Now: func() time.Time { return now }})
	if err != nil {
		return nil, err
	}
	m.lastRun, m.evaluated = now, []string{Category}
	return Findings(rep), nil
}

// LastEvaluatedCategories are the categories the latest Detect evaluated
// (analyzer.EvaluatedCategoryReporter): none between runs or after a
// failure, so open findings resolve only after a completed run.
func (m *Monitor) LastEvaluatedCategories() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.evaluated...)
}
