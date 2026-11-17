package agentposture

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

// The exposure sweep (fleetpol-02a): between analyzer cycles, a cheap read
// of the catalog fingerprint every agents.exposure.sweep_seconds and, when
// it changed (or the database was just enrolled), the exposure detectors at
// once, published through the analyzer's own finding path. It shares the
// Monitor's lock and cadence, so a sweep and a cycle never run together and
// never run the same detectors twice on one catalog.

// ExposureDetectors are the detectors a sweep runs: agent roles with
// dangerous attributes or objects, tables reachable without row-level
// security, always-true policies, definer functions without a search_path,
// PUBLIC grants and agent sessions as owner or service role.
var ExposureDetectors = []string{"AP-01", "AP-02", "AP-03", "AP-04", "AP-05", "AP-15",
	"AP-17"}

// ExposureModeShadow is the only exposure mode of fleetpol-02a: findings
// name the action that would close them, and nothing acts.
const ExposureModeShadow = "shadow"

// DefaultSweepInterval is agents.exposure.sweep_seconds' default.
const DefaultSweepInterval = config.DefaultExposureSweepSeconds * time.Second

// Publisher runs a sweep while no analyzer cycle runs and publishes its
// findings (analyzer.Analyzer).
type Publisher interface {
	RunOutOfCycle(ctx context.Context, fn analyzer.OutOfCycleRun) error
}

// SweepOptions configure RunSweeps.
type SweepOptions struct {
	Publisher Publisher                            // required: without one, no sweep runs
	Interval  func() time.Duration                 // the live sweep_seconds; nil: the default
	After     func(time.Duration) <-chan time.Time // nil: time.After
}

// RunSweeps sweeps once at once (the database was just enrolled), then
// every interval, until ctx ends. Without a publisher it returns at once:
// a sweep whose findings go nowhere would still tell the next cycle that
// the exposure detectors ran.
func (m *Monitor) RunSweeps(ctx context.Context, opts SweepOptions) {
	if opts.Publisher == nil {
		return
	}
	after := opts.After
	if after == nil {
		after = time.After
	}
	for {
		if err := m.SweepOnce(ctx, opts.Publisher); err != nil && ctx.Err() == nil {
			m.opts.Logf("WARN", "agent posture: exposure sweep: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-after(sweepInterval(opts.Interval)):
		}
	}
}

// SweepOnce runs one sweep through pub. When the publish fails, the sweep
// is forgotten, so the next sweep or cycle runs the exposure detectors
// on this catalog again.
func (m *Monitor) SweepOnce(ctx context.Context, pub Publisher) error {
	err := pub.RunOutOfCycle(ctx, m.Sweep)
	if err != nil {
		m.mu.Lock()
		m.exp = cadence{}
		m.mu.Unlock()
	}
	return err
}

// Sweep runs the exposure detectors when the catalog changed since they
// last ran, by a sweep or a full cycle, or never ran. ran is false when
// there was nothing to do.
func (m *Monitor) Sweep(ctx context.Context) (analyzer.OutOfCycle, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pool == nil {
		return analyzer.OutOfCycle{}, false, ErrNoPool
	}
	cfg := m.opts.Config()
	fp, err := m.fingerprint(ctx)
	if err != nil {
		return analyzer.OutOfCycle{}, false, err
	}
	if m.exp.haveFP && m.exp.fp == fp {
		return analyzer.OutOfCycle{}, false, nil
	}
	now := m.opts.Now()
	res, err := RunAll(ctx, m.pool, RunOptions{Registry: exposureOnly(m.registry()),
		Config: cfg, StatementTimeout: m.opts.StatementTimeout, Observations: m.obs})
	if err != nil {
		return analyzer.OutOfCycle{}, false, fmt.Errorf("agent posture sweep: %w", err)
	}
	if !m.logFailures(res.Failed) {
		m.exp.record(fp, now)
	}
	m.logTruncated(res.Outcomes)
	run := analyzer.OutOfCycle{Evaluated: res.EvaluatedCategories(),
		Incomplete: append(res.TruncatedCategories(), failedCategories(res.Failed)...)}
	for _, f := range res.Findings() {
		run.Findings = append(run.Findings, f.AnalyzerFinding())
	}
	m.opts.Logf("INFO", "agent posture: exposure sweep: %d findings from %d detectors",
		len(run.Findings), len(res.Outcomes))
	return run, true, nil
}

// sweepInterval is the live interval, or the default when it is unset or
// outside 15-600 s (validation refuses those; this guards a hot reload).
func sweepInterval(live func() time.Duration) time.Duration {
	if live == nil {
		return DefaultSweepInterval
	}
	d := live()
	if d < config.MinExposureSweepSeconds*time.Second ||
		d > config.MaxExposureSweepSeconds*time.Second {
		return DefaultSweepInterval
	}
	return d
}

// failedCategories are the categories of the detectors that failed.
func failedCategories(failed map[string]error) []string {
	out := make([]string, 0, len(failed))
	for id := range failed {
		out = append(out, CategoryPrefix+id)
	}
	sort.Strings(out)
	return out
}

// exposureOnly is reg's exposure detectors; withoutExposure the rest.
func exposureOnly(reg *Registry) *Registry {
	return reg.filter(func(id string) bool { return slices.Contains(ExposureDetectors, id) })
}

func withoutExposure(reg *Registry) *Registry {
	return reg.filter(func(id string) bool { return !slices.Contains(ExposureDetectors, id) })
}

// filter is a new registry of r's detectors whose id keep accepts.
func (r *Registry) filter(keep func(id string) bool) *Registry {
	out := NewRegistry()
	for _, d := range r.Detectors() {
		if keep(d.Spec().ID) {
			out.byID[d.Spec().ID] = d
		}
	}
	return out
}

// exposureDetail is what the detail of an exposure finding adds: the
// mode, the action fleetpol-02b would take and what SQL cannot see.
func exposureDetail(detector string, detail map[string]any) {
	switch detector {
	case "AP-03":
		detail["would_be_action"] = "enable_row_level_security"
	case "AP-15":
		detail["would_be_action"] = "revoke_public_grant"
	case "AP-04", "AP-05", "AP-17":
	default:
		return
	}
	detail["mode"] = ExposureModeShadow
	detail["not_visible"] = "Read from SQL only: whether the database is reachable from " +
		"the network, and who holds its keys or connection strings, cannot be seen here."
}
