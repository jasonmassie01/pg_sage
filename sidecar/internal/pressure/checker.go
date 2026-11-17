package pressure

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/pg-sage/sidecar/internal/policy"
)

// Source reads one database's inputs. Read resolves table ("" for none)
// and returns ErrUnknownTable when it does not exist; an input it cannot
// read is Unknown in the result, not an error. WALPosition reads the WAL
// insert position alone, for the checker's first rate measurement.
type Source interface {
	Read(ctx context.Context, table string) (Readings, error)
	WALPosition(ctx context.Context) Reading
}

// StorageSource is the cloud adapter's free and total storage in bytes
// and when they were sampled.
type StorageSource interface {
	FreeStorage(ctx context.Context) (free, capacity float64, at time.Time, err error)
}

// Options configure a Checker; zero durations are the defaults.
type Options struct {
	Database string
	Source   Source
	// Storage is the cloud storage adapter; nil leaves free storage
	// unknown.
	Storage StorageSource
	// Limits reads the database's limits (its policy's pressure block);
	// nil is DefaultLimits(Alerts). A failure stops the answer.
	Limits func(context.Context) (Limits, error)
	Alerts Alerts
	// CacheFor is how long one table's answer is reused (default 2 s), so
	// a tight loop cannot load the server; it is also the minimum sleep.
	CacheFor time.Duration
	// WALWindow is the span the WAL rate is measured over (default 60 s);
	// WALFirstSpan the span of a first measurement (default 500 ms).
	WALWindow    time.Duration
	WALFirstSpan time.Duration
	// MaxSleep caps the suggested sleep (default 60 s); ReadTimeout bounds
	// one read set (default 10 s).
	MaxSleep    time.Duration
	ReadTimeout time.Duration
	Now         func() time.Time
	Sleep       func(context.Context, time.Duration) error
}

// Checker answers check_pressure for one database.
type Checker struct {
	o       Options
	mu      sync.Mutex
	entries map[string]*entry
	wal     []walSample
	lag     *lagSample
	rate    float64
	rateOK  bool
}

type entry struct {
	done  chan struct{}
	v     Verdict
	err   error
	until time.Time
}

type walSample struct {
	at  time.Time
	pos float64
}

type lagSample struct {
	at  time.Time
	lag float64
}

const (
	maxTableName   = 260
	maxJobName     = 128
	maxEntries     = 1024
	maxWALSamples  = 512
	minLagInterval = time.Second
)

// NewChecker returns a checker with o's zero fields defaulted.
func NewChecker(o Options) *Checker {
	o.CacheFor = orDuration(o.CacheFor, 2*time.Second)
	o.WALWindow = orDuration(o.WALWindow, time.Minute)
	o.WALFirstSpan = orDuration(o.WALFirstSpan, 500*time.Millisecond)
	o.MaxSleep = orDuration(o.MaxSleep, defaultMaxSleep)
	o.ReadTimeout = orDuration(o.ReadTimeout, 10*time.Second)
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = sleepContext
	}
	return &Checker{o: o, entries: map[string]*entry{}}
}

func orDuration(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Check answers req, from the cache when the table was read within the
// cache period; concurrent calls for one table share one read set. A
// caller's cancellation ends its wait, not the shared read.
func (c *Checker) Check(ctx context.Context, req Request) (Verdict, error) {
	if c == nil || c.o.Source == nil {
		return Verdict{}, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return Verdict{}, err
	}
	if err := validate(req); err != nil {
		return Verdict{}, err
	}
	e, leader := c.claim(req.Table)
	if leader {
		c.fill(ctx, e, req.Table)
	}
	select {
	case <-e.done:
	case <-ctx.Done():
		return Verdict{}, ctx.Err()
	}
	if e.err != nil {
		return Verdict{}, e.err
	}
	return c.answer(e.v, req), nil
}

func validate(req Request) error {
	switch {
	case len(req.Table) > maxTableName:
		return fmt.Errorf("%w: table name longer than %d bytes", ErrInvalidRequest,
			maxTableName)
	case strings.ContainsFunc(req.Table, unicode.IsControl):
		return fmt.Errorf("%w: table name has a control character", ErrInvalidRequest)
	case len(req.Job) > maxJobName:
		return fmt.Errorf("%w: job name longer than %d bytes", ErrInvalidRequest, maxJobName)
	case strings.ContainsFunc(req.Job, unicode.IsControl):
		return fmt.Errorf("%w: job name has a control character", ErrInvalidRequest)
	case req.Batch < 0:
		return fmt.Errorf("%w: batch must be 0 (none) or positive", ErrInvalidRequest)
	}
	return nil
}

// answer is the shared verdict for one caller: its identity and batch.
func (c *Checker) answer(v Verdict, req Request) Verdict {
	v.Database = c.o.Database
	if v.Database == "" {
		v.Database = req.Database
	}
	v.Table, v.Job = req.Table, req.Job
	v.SuggestedBatch = suggestBatch(v.Verdict, v.BatchFactor, req.Batch)
	v.Reasons, v.Inputs = slices.Clone(v.Reasons), slices.Clone(v.Inputs)
	return v
}

// claim returns table's entry and whether this caller must fill it: a
// fresh or in-flight entry is shared, an expired one is replaced.
func (c *Checker) claim(table string) (*entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.o.Now()
	if e, ok := c.entries[table]; ok && (!isDone(e) || now.Before(e.until)) {
		return e, false
	}
	if len(c.entries) >= maxEntries {
		maps.DeleteFunc(c.entries, func(_ string, e *entry) bool {
			return isDone(e) && !now.Before(e.until)
		})
	}
	e := &entry{done: make(chan struct{})}
	c.entries[table] = e
	return e, true
}

func isDone(e *entry) bool {
	select {
	case <-e.done:
		return true
	default:
		return false
	}
}

// fill reads table under a context detached from the caller's, so one
// caller leaving does not fail the others, and publishes the result.
func (c *Checker) fill(parent context.Context, e *entry, table string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), c.o.ReadTimeout)
	defer cancel()
	v, err := c.read(ctx, table)
	c.mu.Lock()
	e.v, e.err, e.until = v, err, c.o.Now().Add(c.o.CacheFor)
	c.mu.Unlock()
	close(e.done)
}

// read is one read set and its answer, for no particular batch.
func (c *Checker) read(ctx context.Context, table string) (Verdict, error) {
	r, err := c.o.Source.Read(ctx, table)
	switch {
	case errors.Is(err, ErrUnknownTable), errors.Is(err, ErrInvalidRequest):
		return Verdict{}, err
	case err != nil:
		return Verdict{}, fmt.Errorf("read pressure inputs of database %s: %w",
			c.o.Database, err)
	}
	readings := maps.Clone(r.Inputs)
	if readings == nil {
		readings = map[string]Reading{}
	}
	readings[policy.PressureWALRate] = c.walRate(ctx, r.WALPosition)
	readings[policy.PressureReplicaLag] = c.boundLag(readings[policy.PressureReplicaLag],
		r.ReplayPosition)
	readings[policy.PressureFreeStorage] = c.storage(ctx)
	rate, rateOK := c.catchUp(readings[policy.PressureReplicaLag])
	limits, limitsErr := c.limits(ctx)
	now := c.o.Now()
	v := Evaluate(Evaluation{Readings: readings, Limits: limits,
		XIDFreezeMax: r.XIDFreezeMax, MXIDFreezeMax: r.MXIDFreezeMax,
		CatchUp: rate, CatchUpKnown: rateOK, MinSleep: c.o.CacheFor,
		MaxSleep: c.o.MaxSleep, Now: now})
	if limitsErr != nil {
		v.Verdict, v.BatchFactor = Stop, 0
		v.SleepSeconds = max(v.SleepSeconds, c.o.CacheFor.Seconds())
		v.Reasons = append(v.Reasons, "the pressure limits could not be read, so the "+
			"answer stops: "+limitsErr.Error())
	}
	v.CheckedAt, v.CachedUntil = now, now.Add(c.o.CacheFor)
	return v, nil
}

// limits are the database's limits; on a failure, the defaults (so every
// input is still reported) and the error.
func (c *Checker) limits(ctx context.Context) (Limits, error) {
	if c.o.Limits == nil {
		return DefaultLimits(c.o.Alerts), nil
	}
	l, err := c.o.Limits(ctx)
	if err != nil {
		return DefaultLimits(c.o.Alerts), err
	}
	return l, nil
}

// storage is free storage as a percentage of capacity.
func (c *Checker) storage(ctx context.Context) Reading {
	if c.o.Storage == nil {
		return Unknown("no cloud storage adapter for this database (free storage is read " +
			"from the provider's API)")
	}
	free, capacity, at, err := c.o.Storage.FreeStorage(ctx)
	switch {
	case err != nil:
		return Unknown("free storage not read: " + err.Error())
	case capacity <= 0:
		return Unknown("storage capacity unknown or unbounded")
	}
	return Known(100*free/capacity, at)
}
