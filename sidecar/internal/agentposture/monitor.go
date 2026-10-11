package agentposture

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

// MonitorOptions configure a Monitor.
type MonitorOptions struct {
	Registry         *Registry                    // nil: Default()
	Config           func() Config                // the live agents.* keys; nil: DefaultConfig
	Now              func() time.Time             // nil: time.Now
	StatementTimeout time.Duration                // 0: DefaultStatementTimeout
	Logf             func(string, string, ...any) // level, format, args; nil: silent
}

// Monitor runs posture in the analyzer (an analyzer.SupplementalDetector):
// on its first cycle, when the catalog posture reads changes (Guard's own
// fingerprint of it, spec §6.15) and once a day at agents.posture.daily_at.
// Other cycles read only the fingerprint and report nothing, so the open
// posture findings stay as they are.
type Monitor struct {
	pool      *pgxpool.Pool
	opts      MonitorOptions
	mu        sync.Mutex
	st        cadence
	evaluated []string
	obs       *ObservationStore
}

// NewMonitor returns a monitor of pool's database.
func NewMonitor(pool *pgxpool.Pool, opts MonitorOptions) *Monitor {
	if opts.Config == nil {
		opts.Config = DefaultConfig
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logf == nil {
		opts.Logf = func(string, string, ...any) {}
	}
	return &Monitor{pool: pool, opts: opts, obs: NewObservationStore()}
}

// Detect runs posture when it is due and returns its findings.
func (m *Monitor) Detect(ctx context.Context) ([]analyzer.Finding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.evaluated = nil
	if m.pool == nil {
		return nil, ErrNoPool
	}
	cfg := m.opts.Config()
	fp, err := m.fingerprint(ctx)
	if err != nil {
		return nil, err
	}
	now := m.opts.Now()
	reason := m.st.dueReason(fp, now, cfg.DailyAt)
	if reason == "" {
		return nil, nil
	}
	res, err := RunAll(ctx, m.pool, RunOptions{Registry: m.opts.Registry, Config: cfg,
		StatementTimeout: m.opts.StatementTimeout, Observations: m.obs})
	if err != nil {
		return nil, fmt.Errorf("agent posture (%s): %w", reason, err)
	}
	if !m.logFailures(res.Failed) {
		m.st.record(fp, now)
	}
	m.evaluated = res.EvaluatedCategories()
	found := res.Findings()
	out := make([]analyzer.Finding, len(found))
	for i, f := range found {
		out[i] = f.AnalyzerFinding()
	}
	m.opts.Logf("INFO", "agent posture (%s): %d findings from %d detectors", reason,
		len(out), len(res.Outcomes))
	return out, nil
}

// logFailures logs each failed detector and reports whether any failure
// was transient; then the run is repeated on the next cycle.
func (m *Monitor) logFailures(failed map[string]error) bool {
	ids := make([]string, 0, len(failed))
	for id := range failed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	again := false
	for _, id := range ids {
		m.opts.Logf("WARN", "agent posture: %s did not complete: %v", id, failed[id])
		again = again || transient(failed[id])
	}
	return again
}

// LastEvaluatedCategories are the categories the latest Detect evaluated
// (analyzer.EvaluatedCategoryReporter): none when posture was not due or
// failed, so open findings resolve only after a completed check.
func (m *Monitor) LastEvaluatedCategories() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.evaluated...)
}

func (m *Monitor) fingerprint(ctx context.Context) (int64, error) {
	tx, err := beginRead(ctx, m.pool, m.opts.StatementTimeout)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }() // read-only
	return Fingerprint(ctx, tx)
}

// Due reasons.
const (
	dueFirst   = "first run"
	dueCatalog = "catalog changed"
	dueDaily   = "daily"
)

// cadence is when posture last ran and on which catalog fingerprint.
type cadence struct {
	ran    time.Time
	fp     int64
	haveFP bool
}

// dueReason says why posture is due now, or "" when it is not.
func (c cadence) dueReason(fp int64, now time.Time, dailyAt string) string {
	if c.ran.IsZero() {
		return dueFirst
	}
	if !c.haveFP || fp != c.fp {
		return dueCatalog
	}
	boundary, err := dailyBoundary(now, dailyAt)
	if err != nil {
		boundary = now.Add(-24 * time.Hour)
		if !c.ran.After(boundary) {
			return dueDaily
		}
		return ""
	}
	if c.ran.Before(boundary) {
		return dueDaily
	}
	return ""
}

func (c *cadence) record(fp int64, now time.Time) {
	c.ran, c.fp, c.haveFP = now, fp, true
}

// dailyBoundary is the latest daily_at at or before now, in now's zone.
func dailyBoundary(now time.Time, dailyAt string) (time.Time, error) {
	h, m, err := config.ParseDailyAt(dailyAt)
	if err != nil {
		return time.Time{}, err
	}
	b := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if now.Before(b) {
		b = b.AddDate(0, 0, -1)
	}
	return b, nil
}
