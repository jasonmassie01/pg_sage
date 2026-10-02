package sre

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// ReactiveDetector is the deterministic trigger (AI-SRE-SPEC §2.1) for
// the M6 families that have no RCA signal of their own: once per
// trigger poll it samples the cumulative checkpoint and temp-file
// counters and the LWLock waits through catalog probes, keeps a short
// history in memory and opens one investigation per episode. Detection
// never depends on an LLM. Thresholds are conservative product
// defaults (DefaultDetectorConfig); a restart forgets the history, so
// the first poll after it never fires.
type ReactiveDetector struct {
	runner   ProbeRunner
	database string
	cfg      DetectorConfig
	logFn    func(level, msg string, args ...any)
	notices  OnceLog

	mu    sync.Mutex
	ckpt  []probes.CheckpointStat
	ckAt  []time.Time
	temp  []probes.TempStat
	tmAt  []time.Time
	hot   int // consecutive polls with a sustained LWLock class
	state map[TriggerKind]*episode
}

// DetectorConfig is the detector's thresholds.
type DetectorConfig struct {
	// Window is how far back counter growth is measured.
	Window time.Duration
	// CheckpointRequested is the requested checkpoints within Window
	// that are a storm (and they must outnumber timed ones).
	CheckpointRequested float64
	// TempBytes is this database's temp-file bytes within Window.
	TempBytes float64
	// LWLockWaiters on one modeled LWLock class in LWLockPolls
	// consecutive polls is sustained contention.
	LWLockWaiters int64
	LWLockPolls   int
	// Cooldown spaces two episodes of one family.
	Cooldown time.Duration
}

// DefaultDetectorConfig is the conservative default: 3 requested
// checkpoints within 5 minutes (PostgreSQL warns at one per 30 s), 1 GiB
// of temp files within 5 minutes, 8 backends on one LWLock class in 3
// consecutive polls, and 30 minutes between episodes of a family.
func DefaultDetectorConfig() DetectorConfig {
	return DetectorConfig{Window: 5 * time.Minute, CheckpointRequested: 3,
		TempBytes: 1 << 30, LWLockWaiters: 8, LWLockPolls: 3, Cooldown: 30 * time.Minute}
}

// Validate checks the thresholds.
func (c DetectorConfig) Validate() error {
	switch {
	case c.Window <= 0:
		return fmt.Errorf("%w: detector window must be positive", ErrInvalidRequest)
	case c.CheckpointRequested < 1:
		return fmt.Errorf("%w: requested checkpoints must be at least 1", ErrInvalidRequest)
	case c.TempBytes <= 0:
		return fmt.Errorf("%w: temp bytes must be positive", ErrInvalidRequest)
	case c.LWLockWaiters < 1 || c.LWLockPolls < 1:
		return fmt.Errorf("%w: LWLock waiters and polls must be at least 1",
			ErrInvalidRequest)
	case c.Cooldown < 0:
		return fmt.Errorf("%w: cooldown must not be negative", ErrInvalidRequest)
	}
	return nil
}

// episode is one family's current or last detection.
type episode struct {
	active bool
	start  time.Time
	fired  time.Time
}

// NewReactiveDetector samples database through runner; logFn may be nil.
func NewReactiveDetector(runner ProbeRunner, database string, cfg DetectorConfig,
	logFn func(level, msg string, args ...any)) (*ReactiveDetector, error) {
	if runner == nil || database == "" {
		return nil, fmt.Errorf("%w: detector needs a probe runner and a database name",
			ErrInvalidRequest)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logFn == nil {
		logFn = func(string, string, ...any) {}
	}
	return &ReactiveDetector{runner: runner, database: database, cfg: cfg, logFn: logFn,
		state: map[TriggerKind]*episode{}}, nil
}

// Triggers samples once and returns the triggers of the families whose
// episode is open. Polls are serialized.
func (d *ReactiveDetector) Triggers(ctx context.Context) ([]Trigger, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []Trigger
	for _, f := range []struct {
		kind TriggerKind
		poll func(context.Context) (bool, time.Time, bool)
	}{{TriggerCheckpoint, d.pollCheckpoint}, {TriggerTempFiles, d.pollTemp},
		{TriggerLWLock, d.pollLWLock}} {
		cond, at, ok := f.poll(ctx)
		if !ok {
			continue
		}
		if t, fire := d.step(f.kind, cond, at); fire {
			out = append(out, t)
		}
	}
	return out, ctx.Err()
}

// step advances a family's episode and returns its trigger while the
// episode is open. A new episode inside the cooldown is suppressed.
func (d *ReactiveDetector) step(kind TriggerKind, cond bool, at time.Time) (Trigger, bool) {
	e := d.state[kind]
	if e == nil {
		e = &episode{}
		d.state[kind] = e
	}
	if !cond {
		e.active = false
		return Trigger{}, false
	}
	if !e.active {
		if !e.fired.IsZero() && at.Sub(e.fired) < d.cfg.Cooldown {
			return Trigger{}, false
		}
		e.active, e.start, e.fired = true, at, at
	}
	return Trigger{CaseID: clip(fmt.Sprintf("sre:detector:%s:%s", kind, d.database)),
		Kind: kind, Subject: detectorSubjects[kind],
		IdempotencyKey: fmt.Sprintf("detector:%s:%d", kind, e.start.Unix())}, true
}

var detectorSubjects = map[TriggerKind]string{TriggerCheckpoint: "requested checkpoints",
	TriggerTempFiles: "temp-file growth", TriggerLWLock: "LWLock contention"}

// unavailable logs a probe that could not be read, once per probe and
// reason.
func (d *ReactiveDetector) unavailable(res probes.Result, err error) {
	key := string(res.ProbeID) + "/" + string(res.Status) + "/" + res.Reason
	d.notices.Log(d.logFn, key, "WARN", "sre: db %q: the reactive detector cannot read %s "+
		"(%s %s): %v; that family is not detected", d.database, res.ProbeID, res.Status,
		res.Reason, err)
}

func (d *ReactiveDetector) pollCheckpoint(ctx context.Context) (bool, time.Time, bool) {
	res := d.runner.Run(ctx, probes.CheckpointActivity, probes.Args{})
	s, err := probes.CheckpointStats(res)
	if err != nil {
		d.unavailable(res, err)
		return false, time.Time{}, false
	}
	at := res.ObservedAt
	if n := len(d.ckpt); n > 0 && ckptReset(d.ckpt[n-1], s) {
		d.ckpt, d.ckAt = nil, nil
	}
	d.ckpt, d.ckAt = append(d.ckpt, s), append(d.ckAt, at)
	i := firstInWindow(d.ckAt, at, d.cfg.Window)
	d.ckpt, d.ckAt = d.ckpt[i:], d.ckAt[i:]
	base := d.ckpt[0]
	req, ok1 := probes.Delta(base.Requested, s.Requested)
	timed, ok2 := probes.Delta(base.Timed, s.Timed)
	return ok1 && ok2 && req >= d.cfg.CheckpointRequested && req > timed, at, true
}

func ckptReset(prev, cur probes.CheckpointStat) bool {
	_, ok1 := probes.Delta(prev.Requested, cur.Requested)
	_, ok2 := probes.Delta(prev.Timed, cur.Timed)
	return !ok1 || !ok2 || !prev.StatsReset.Equal(cur.StatsReset) ||
		!prev.ServerStartedAt.Equal(cur.ServerStartedAt)
}

func (d *ReactiveDetector) pollTemp(ctx context.Context) (bool, time.Time, bool) {
	res := d.runner.Run(ctx, probes.TempFileActivity, probes.Args{})
	s, err := probes.TempStats(res)
	if err != nil {
		d.unavailable(res, err)
		return false, time.Time{}, false
	}
	at := res.ObservedAt
	if n := len(d.temp); n > 0 {
		prev := d.temp[n-1]
		if _, ok := probes.Delta(prev.Bytes, s.Bytes); !ok ||
			!prev.StatsReset.Equal(s.StatsReset) ||
			!prev.ServerStartedAt.Equal(s.ServerStartedAt) {
			d.temp, d.tmAt = nil, nil
		}
	}
	d.temp, d.tmAt = append(d.temp, s), append(d.tmAt, at)
	i := firstInWindow(d.tmAt, at, d.cfg.Window)
	d.temp, d.tmAt = d.temp[i:], d.tmAt[i:]
	grew, ok := probes.Delta(d.temp[0].Bytes, s.Bytes)
	return ok && grew >= d.cfg.TempBytes, at, true
}

// firstInWindow is the index of the oldest time within window of now.
func firstInWindow(ts []time.Time, now time.Time, window time.Duration) int {
	for i, t := range ts {
		if now.Sub(t) <= window {
			return i
		}
	}
	return len(ts) - 1
}

func (d *ReactiveDetector) pollLWLock(ctx context.Context) (bool, time.Time, bool) {
	res := d.runner.Run(ctx, probes.LWLockWaits, probes.Args{})
	gs, err := probes.WaitGroups(res)
	if err != nil {
		d.unavailable(res, err)
		d.hot = 0
		return false, time.Time{}, false
	}
	classes := map[causal.NodeID]int64{}
	var peak int64
	for _, g := range gs {
		if id, ok := causal.LWLockClass(g.Event); ok && g.Type == "LWLock" {
			classes[id] += g.Backends
			peak = max(peak, classes[id])
		}
	}
	if peak >= d.cfg.LWLockWaiters {
		d.hot++
	} else {
		d.hot = 0
	}
	return d.hot >= d.cfg.LWLockPolls, res.ObservedAt, true
}
