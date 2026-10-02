package runway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Starter starts (or coalesces into) an investigation (sre.Coordinator).
type Starter interface {
	Start(ctx context.Context, t sre.Trigger) (sre.Investigation, bool, error)
}

// Monitor samples, evaluates and reports one database's runways.
type Monitor struct {
	pool    *pgxpool.Pool
	runner  ProbeRunner
	starter Starter
	opts    Options
	logFn   func(level, msg string, args ...any)

	mu     sync.Mutex
	last   map[seriesKey]lastPoint
	loaded bool
}

// TickResult is what one tick did.
type TickResult struct {
	Sampled  int
	Runways  []Runway
	Started  int
	Standby  bool
	Resolved []string // categories whose cleared findings were resolved
}

// NewMonitor validates its dependencies. starter may be nil (findings
// only, no investigations).
func NewMonitor(pool *pgxpool.Pool, runner ProbeRunner, starter Starter, opts Options,
	logFn func(level, msg string, args ...any)) (*Monitor, error) {
	if pool == nil || runner == nil {
		return nil, errors.New("runway monitor needs a database and a probe runner")
	}
	if opts.Interval <= 0 || opts.Lookback <= 0 || opts.Retention < opts.Lookback ||
		opts.MinSamples < 3 {
		return nil, fmt.Errorf("runway monitor options are invalid: %+v", opts)
	}
	if logFn == nil {
		logFn = func(string, string, ...any) {}
	}
	return &Monitor{pool: pool, runner: runner, starter: starter, opts: opts,
		logFn: logFn, last: map[seriesKey]lastPoint{}}, nil
}

// Run ticks every interval until ctx ends. A failed tick is logged and
// retried on the next one.
func (m *Monitor) Run(ctx context.Context) {
	t := time.NewTicker(m.opts.Interval)
	defer t.Stop()
	for {
		if _, err := m.Tick(ctx); err != nil && ctx.Err() == nil {
			m.logFn("WARN", "runway: db %q: %v", m.opts.Database, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick samples the series, evaluates the runways over the lookback,
// updates the forecast findings and starts the pre-incident
// investigations. It reports the first problem met; what could be done
// is done.
func (m *Monitor) Tick(ctx context.Context) (TickResult, error) {
	var res TickResult
	snap, problems := m.read(ctx)
	res.Standby = snap.WAL != nil && snap.WAL.InRecovery
	if !res.Standby {
		n, err := m.sample(ctx, snap)
		res.Sampled, problems = n, append(problems, err)
	}
	trends, err := probes.RunwayTrends(m.runner.Run(ctx, probes.RunwayTrendsProbe,
		probes.Args{Window: m.opts.Lookback}))
	if err != nil {
		problems = append(problems, fmt.Errorf("runway_trends: %w", err))
	}
	in := EvalInput{Trends: trends, TrendsOK: err == nil, Tables: snap.Tables,
		TablesOK: snap.TablesOK, Sequences: snap.Sequences, SequencesOK: snap.SequencesOK}
	runways, evaluated := Evaluate(in, m.opts)
	res.Runways = runways
	if !res.Standby {
		res.Started, res.Resolved, err = m.report(ctx, runways, evaluated)
		problems = append(problems, err)
	}
	return res, errors.Join(problems...)
}

// read runs the current-state probes; an unreadable one is reported and
// left out of the snapshot.
func (m *Monitor) read(ctx context.Context) (Snapshot, []error) {
	var s Snapshot
	var errs []error
	run := func(id probes.ID) probes.Result { return m.runner.Run(ctx, id, probes.Args{}) }
	note := func(id probes.ID, err error) bool {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
		}
		return err == nil
	}
	if x, err := probes.XIDRunwayOf(run(probes.XIDRunwayProbe)); note(probes.XIDRunwayProbe,
		err) {
		s.XID = &x
	}
	if w, err := probes.WALRunwayOf(run(probes.WALRunwayProbe)); note(probes.WALRunwayProbe,
		err) {
		s.WAL = &w
	}
	if d, err := probes.WALDirectoryOf(run(probes.WALDirectoryProbe)); err == nil {
		s.Dir = &d // without pg_monitor disk usage is simply not sampled
	}
	var err error
	s.Tables, err = probes.WraparoundTables(run(probes.WraparoundTablesProbe))
	s.TablesOK = note(probes.WraparoundTablesProbe, err)
	s.Slots, err = probes.Slots(run(probes.ReplicationSlots))
	s.SlotsOK = note(probes.ReplicationSlots, err)
	s.Sequences, err = probes.Sequences(run(probes.SequenceRunwayProbe))
	s.SequencesOK = note(probes.SequenceRunwayProbe, err)
	return s, errs
}
