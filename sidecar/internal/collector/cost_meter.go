package collector

import (
	"context"
	"errors"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// The cost meter records what the collector's catalog statements really
// spend (footprint-05): per cycle, the summed execution time of every
// bounded read, the slowest one with the step that ran it, and the reads
// cut off by the server's statement or lock timeout. Self-configuration
// sizes the collector interval and the read deadline from the last
// costWindow cycles instead of from a proxy scan.

// costWindow cycles are kept: one hour at the default 60 s interval, the
// period of the live derivation pass.
const costWindow = 60

// unnamedStep names a statement made outside a catalog step.
const unnamedStep = "cycle"

// CycleCost is what the catalog statements of the last cycles (at most
// costWindow) spent on the database.
type CycleCost struct {
	Cycles int // cycles in the window
	// P90CycleMs is the 90th percentile of each cycle's summed statement
	// execution time; P90SlowestMs that of each cycle's slowest statement.
	P90CycleMs   float64
	P90SlowestMs float64
	// SlowestMs is the slowest statement in the window; SlowestStep is the
	// catalog step that ran it.
	SlowestMs   float64
	SlowestStep string
	// Timeouts counts statements cut off by the statement or lock timeout
	// (57014, 55P03) in the window; TimeoutSteps names their steps.
	Timeouts     int
	TimeoutSteps []string
	LastCycle    time.Time
}

type cycleSample struct {
	at           time.Time
	totalMs      float64
	slowestMs    float64
	slowestStep  string
	timeouts     int
	timeoutSteps []string
}

// costMeter is safe for concurrent use; its zero value is ready.
type costMeter struct {
	mu     sync.Mutex
	active bool
	cur    cycleSample
	ring   [costWindow]cycleSample
	n      int // samples in ring
	next   int // where the next one goes
}

// start begins metering a cycle.
func (m *costMeter) start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active, m.cur = true, cycleSample{}
}

// abandon drops the cycle being metered (a skipped cycle is not a cycle).
func (m *costMeter) abandon() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active = false
}

// finish keeps the cycle being metered in the window.
func (m *costMeter) finish(at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.active {
		return
	}
	m.cur.at = at
	m.ring[m.next] = m.cur
	m.next = (m.next + 1) % costWindow
	m.n = min(m.n+1, costWindow)
	m.active = false
}

// record adds one statement to the cycle being metered (catalogread
// Reader.Done); a statement outside a cycle is not counted.
func (m *costMeter) record(ctx context.Context, s catalogread.ReadStat) {
	step := stepOf(ctx)
	if step == "" {
		step = unnamedStep
	}
	ms := float64(s.Exec.Microseconds()) / 1000
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.active {
		return
	}
	m.cur.totalMs += ms
	if ms > m.cur.slowestMs || m.cur.slowestStep == "" {
		m.cur.slowestMs, m.cur.slowestStep = ms, step
	}
	if isServerTimeout(s.Err) {
		m.cur.timeouts++
		if !slices.Contains(m.cur.timeoutSteps, step) {
			m.cur.timeoutSteps = append(m.cur.timeoutSteps, step)
		}
	}
}

// summary is the window's cost (false before the first full cycle).
func (m *costMeter) summary() (CycleCost, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.n == 0 {
		return CycleCost{}, false
	}
	out := CycleCost{Cycles: m.n}
	totals := make([]float64, 0, m.n)
	slowest := make([]float64, 0, m.n)
	steps := map[string]bool{}
	for i := 0; i < m.n; i++ {
		s := m.ring[i]
		totals, slowest = append(totals, s.totalMs), append(slowest, s.slowestMs)
		if s.slowestStep != "" && (out.SlowestStep == "" || s.slowestMs > out.SlowestMs) {
			out.SlowestMs, out.SlowestStep = s.slowestMs, s.slowestStep
		}
		out.Timeouts += s.timeouts
		for _, st := range s.timeoutSteps {
			steps[st] = true
		}
		if s.at.After(out.LastCycle) {
			out.LastCycle = s.at
		}
	}
	out.P90CycleMs, out.P90SlowestMs = p90(totals), p90(slowest)
	for st := range steps {
		out.TimeoutSteps = append(out.TimeoutSteps, st)
	}
	sort.Strings(out.TimeoutSteps)
	return out, true
}

// p90 is the nearest-rank 90th percentile of a non-empty xs.
func p90(xs []float64) float64 {
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	return sorted[(9*len(sorted)+9)/10-1]
}

// isServerTimeout: the server cut the statement off at its statement
// (57014) or lock (55P03) timeout.
func isServerTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "57014" || pgErr.Code == "55P03")
}

// CycleCost is what the collector's catalog statements spent over its
// last cycles; false before the first full cycle (and on a nil collector).
func (c *Collector) CycleCost() (CycleCost, bool) {
	if c == nil {
		return CycleCost{}, false
	}
	return c.meter.summary()
}

type stepKey struct{}

// withStep names the catalog step whose statements run under ctx.
func withStep(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, stepKey{}, name)
}

// stepOf is the catalog step running under ctx ("" outside one).
func stepOf(ctx context.Context) string {
	name, _ := ctx.Value(stepKey{}).(string)
	return name
}
