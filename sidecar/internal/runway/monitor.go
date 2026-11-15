package runway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/selfbudget"
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

	now func() time.Time
	// seqMu serializes sequence reads; seq is the last reading.
	seqMu sync.Mutex
	seq   sequenceReading
	// keepMu guards seqKeep, the sequences with samples in the lookback
	// that the next sequence reading lists whatever their rank.
	keepMu  sync.Mutex
	seqKeep []string
	// sizeMu guards sizeSeq, the sequence of the last size this monitor
	// sampled (zero: none yet).
	sizeMu  sync.Mutex
	sizeSeq uint64
	// staleMu guards stale, the held findings this monitor marked stale
	// (category NUL identifier), and truncated, whether the last trends
	// read was cut at its row cap: each transition is logged once.
	staleMu   sync.Mutex
	stale     map[string]bool
	truncated bool
	// ownMu guards ownKey, the cluster key this monitor claims in
	// opts.Owners, and noOwnerKey, whether its system identifier was
	// unreadable at the last reading (logged once).
	ownMu      sync.Mutex
	ownKey     string
	noOwnerKey bool
	// mxMu serializes the multixact member readings (guard-02a) and
	// guards mxAt and mxLast, the last reading and when it was made, and
	// mxStatus, the last reading's status (each change is logged once).
	// mxRead makes a reading and mxLimits are its runway's limits.
	mxMu     sync.Mutex
	mxAt     time.Time
	mxLast   *MXMemberResult
	mxStatus string
	mxRead   func(context.Context) (MXMemberEstimate, error)
	mxLimits mxLimits
}

// TickResult is what one tick did.
type TickResult struct {
	Sampled  int
	Runways  []Runway
	Started  int
	Standby  bool
	Resolved []string // categories whose cleared findings were resolved
	// ClusterOwner is the database reporting the cluster series ("" when
	// this monitor reports them on its own); Duplicates counts the
	// cluster findings this non-owner resolved because the owner did.
	ClusterOwner string
	Duplicates   int
	// MXMembers is the multixact member space reading (nil on a standby
	// and on a database that does not report the cluster's series).
	MXMembers *MXMemberResult
}

// NewMonitor validates its dependencies. starter may be nil (findings
// only, no investigations).
func NewMonitor(pool *pgxpool.Pool, runner ProbeRunner, starter Starter, opts Options,
	logFn func(level, msg string, args ...any)) (*Monitor, error) {
	if pool == nil || runner == nil {
		return nil, errors.New("runway monitor needs a database and a probe runner")
	}
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if logFn == nil {
		logFn = func(string, string, ...any) {}
	}
	if opts.Sizes == nil && opts.SizeInterval > 0 {
		opts.Sizes = NewSizeShare() // its own reading, kept for SizeInterval
	}
	return &Monitor{pool: pool, runner: runner, starter: starter, opts: opts,
		logFn: logFn, last: map[seriesKey]lastPoint{}, now: time.Now,
		stale: map[string]bool{}, mxLimits: defaultMXLimits,
		mxRead: func(ctx context.Context) (MXMemberEstimate, error) {
			return measureMXMembers(ctx, pool)
		}}, nil
}

// Run ticks every interval until ctx ends. A failed tick is logged and
// retried on the next one.
func (m *Monitor) Run(ctx context.Context) {
	t := time.NewTicker(m.opts.Interval)
	defer t.Stop()
	defer m.leaveCluster()
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
// updates the forecast findings (holding open, marked stale, those whose
// series it could not measure) and starts the pre-incident
// investigations. It reports the first problem met; what could be done
// is done.
func (m *Monitor) Tick(ctx context.Context) (TickResult, error) {
	ctx, done := selfbudget.Process().TrackFor(ctx, "runways")
	defer done()
	var res TickResult
	snap, problems := m.read(ctx)
	res.Standby = snap.WAL != nil && snap.WAL.InRecovery
	if res.Standby {
		m.claimCluster(snap) // a standby drops its claim
	} else {
		n, err := m.sample(ctx, snap)
		res.Sampled, problems = n, append(problems, err)
	}
	set, err := probes.RunwayTrendsOf(m.runner.Run(ctx, probes.RunwayTrendsProbe,
		probes.Args{Window: m.opts.Lookback}))
	if err != nil {
		problems = append(problems, fmt.Errorf("runway_trends: %w", err))
	} else {
		m.noteTruncation(set)
		m.noteSequenceSeries(set)
	}
	in := EvalInput{XID: snap.XID, Trends: set.Trends, TrendsOK: err == nil,
		TrendsTruncated: set.Truncated, TrendsLastKind: set.LastKind, Tables: snap.Tables,
		TablesOK: snap.TablesOK, Slots: snap.Slots, SlotsOK: snap.SlotsOK,
		Sequences: snap.Sequences, SequencesOK: snap.SequencesOK,
		Capacity: snap.diskCapacity(m.opts)}
	openErr := m.fillOpenSlots(ctx, &in, res.Standby)
	runways, evaluated := Evaluate(in, m.opts)
	if openErr != nil { // fail closed: a slot without samples may still exist
		problems, evaluated = append(problems, openErr), without(evaluated, CategoryWAL)
	}
	if !res.Standby {
		scope := m.claimCluster(snap)
		problems = append(problems, m.reportScoped(ctx, &res, in, runways, evaluated,
			scope, openErr == nil))
		res.MXMembers, err = m.tickMXMembers(ctx, scope)
		problems = append(problems, err)
	} else {
		res.Runways = runways
	}
	return res, errors.Join(problems...)
}

// reportScoped narrows the tick to the monitor's cluster scope, reports
// it, records a complete owner report and resolves a non-owner's
// duplicates once the owner has reported.
func (m *Monitor) reportScoped(ctx context.Context, res *TickResult, in EvalInput,
	runways []Runway, evaluated []string, scope clusterScope, slotsKnown bool) error {
	runways, evaluated, held := scope.apply(runways, evaluated, Holds(in, m.opts))
	res.Runways = runways
	if scope.shared {
		res.ClusterOwner = scope.owner
	}
	var err error
	res.Started, res.Resolved, err = m.report(ctx, runways, evaluated, held, scope)
	if err == nil && scope.shared && scope.isOwner && slotsKnown &&
		clusterEvaluated(in) {
		m.opts.Owners.Evaluated(scope.key, m.opts.Database)
	}
	n, dupErr := m.resolveDuplicates(ctx, scope)
	res.Duplicates = n
	return errors.Join(err, dupErr)
}

// read runs the current-state probes; an unreadable one is reported and
// left out of the snapshot.
func (m *Monitor) read(ctx context.Context) (Snapshot, []error) {
	c := capacity(ctx, m.opts)
	s := Snapshot{Capacity: &c}
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
		s.SizeFresh = m.fillSize(ctx, &w, note)
		s.WAL = &w
	}
	if d, err := probes.WALDirectoryOf(run(probes.WALDirectoryProbe)); err == nil {
		s.Dir = &d // without pg_monitor disk usage is simply not sampled
	}
	// wraparound_tables reads per-table statistics: on PostgreSQL 14 the
	// backend first waits for a fresh stats collector file, which can take
	// seconds on a loaded host, so it samples with its background budget.
	var err error
	s.Tables, err = probes.WraparoundTables(m.runner.RunBackground(ctx,
		probes.WraparoundTablesProbe, probes.Args{}))
	s.TablesOK = note(probes.WraparoundTablesProbe, err)
	s.Slots, err = probes.Slots(run(probes.ReplicationSlots))
	s.SlotsOK = note(probes.ReplicationSlots, err)
	if err := m.readSequences(ctx, &s); err != nil {
		errs = append(errs, err)
	}
	return s, errs
}

// Sample reads the current state and writes one sample of every series,
// without evaluating, reporting or starting anything. PGIncidentBench
// drives it to build trends on a compressed timescale. A standby writes
// nothing.
func (m *Monitor) Sample(ctx context.Context) (int, error) {
	snap, problems := m.read(ctx)
	if snap.WAL != nil && snap.WAL.InRecovery {
		return 0, errors.Join(problems...)
	}
	n, err := m.sample(ctx, snap)
	return n, errors.Join(append(problems, err)...)
}

// fillSize adds the databases' total size to a primary's WAL reading: one
// measurement per cluster per size interval (every tick without one),
// shared with the process's other runtimes on the cluster. It reports
// whether this monitor has not sampled that measurement yet. A standby
// samples nothing, so it measures nothing.
func (m *Monitor) fillSize(ctx context.Context, w *probes.WALRunway,
	note func(probes.ID, error) bool) bool {
	if w.InRecovery {
		return false
	}
	pass := m.opts.SizeInterval
	if pass <= 0 {
		pass = m.opts.Interval
	}
	size, _, err := m.opts.Sizes.Measure(ctx, ClusterKey(*w), pass,
		func(ctx context.Context) (ClusterSize, error) { return measureSize(ctx, m.runner) })
	if !note(probes.ClusterDatabaseSizeProbe, err) {
		return false
	}
	w.DatabaseBytes, w.UnreadableDatabases = size.DatabaseBytes, size.UnreadableDatabases
	m.sizeMu.Lock()
	defer m.sizeMu.Unlock()
	if size.seq == m.sizeSeq {
		return false
	}
	m.sizeSeq = size.seq
	return true
}
