package cloudtel

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/managedparam"
	"github.com/pg-sage/sidecar/internal/verify"
)

// DefaultInterval is the polling period (CloudWatch's basic RDS metrics
// are one-minute points).
const DefaultInterval = time.Minute

const maxHistory = 2000

// RuntimeOptions configure one database's telemetry runtime.
type RuntimeOptions struct {
	Database string
	Interval time.Duration
	Limits   Limits
	Now      func() time.Time
}

// Runtime polls one database's provider telemetry and serves it to the
// guards: executor load admission (CPU, withhold reasons), the memory
// gates and the managed-storage runway. A failed poll fails closed: the
// evidence becomes unknown until the next good sample.
type Runtime struct {
	src      Source
	database string
	provider string
	reason   string // permanently unavailable: why
	interval time.Duration
	limits   Limits
	now      func() time.Time
	pollMu   sync.Mutex

	mu          sync.RWMutex
	sample      *Sample
	lastPoll    time.Time
	lastSuccess time.Time
	lastErr     string
	history     []Point
	capacity    float64
	drift       []managedparam.Drift
}

// NewRuntime builds a polling runtime over src.
func NewRuntime(src Source, opts RuntimeOptions) (*Runtime, error) {
	if src == nil || opts.Database == "" {
		return nil, errors.New("cloud telemetry runtime needs a source and a database")
	}
	if err := opts.Limits.Validate(); err != nil {
		return nil, err
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Runtime{src: src, database: opts.Database, provider: src.Provider(),
		interval: opts.Interval, limits: opts.Limits, now: opts.Now}, nil
}

// Unavailable is a runtime that only reports why telemetry cannot be
// collected for the database.
func Unavailable(database, provider, reason string) *Runtime {
	return &Runtime{database: database, provider: provider, reason: reason,
		interval: DefaultInterval, now: time.Now}
}

// Database is the runtime's database name.
func (r *Runtime) Database() string { return r.database }

// Interval is the polling period.
func (r *Runtime) Interval() time.Duration { return r.interval }

// Poll collects one sample.
func (r *Runtime) Poll(ctx context.Context) error {
	if r.src == nil {
		return fmt.Errorf("%w: %s", ErrUnavailable, r.reason)
	}
	r.pollMu.Lock()
	defer r.pollMu.Unlock()
	now := r.now()
	s, err := r.src.Collect(ctx, now)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastPoll = now
	if err != nil {
		r.sample, r.lastErr = nil, err.Error()
		return err
	}
	r.sample, r.lastErr, r.lastSuccess = &s, "", now
	r.record(s, now)
	return nil
}

// record appends the effective free storage to the runway history; a
// changed capacity (a resize) restarts it.
func (r *Runtime) record(s Sample, now time.Time) {
	free, capacity, ok := s.EffectiveFreeStorage(now)
	if !ok {
		r.history, r.capacity = nil, 0
		return
	}
	if capacity != r.capacity {
		r.history, r.capacity = nil, capacity
	}
	r.history = append(r.history, Point{Value: free, At: s.FreeStorageBytes.At})
	if len(r.history) > maxHistory {
		r.history = append([]Point(nil), r.history[len(r.history)-maxHistory:]...)
	}
}

// Run polls every interval until ctx ends, then invalidates the evidence.
func (r *Runtime) Run(ctx context.Context, report func(error)) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	defer r.invalidate()
	for {
		if err := r.Poll(ctx); err != nil && ctx.Err() == nil && report != nil {
			report(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Runtime) invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sample, r.history, r.capacity = nil, nil, 0
}

func (r *Runtime) current() (*Sample, []Point) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sample, r.history
}

// CurrentCPU is fresh provider CPU for load admission
// (executor.HostCPUReader).
func (r *Runtime) CurrentCPU(ctx context.Context) (float64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s, _ := r.current()
	if s == nil {
		reason := "no sample yet"
		if r.reason != "" {
			reason = r.reason
		}
		return 0, fmt.Errorf("%w: %w: %s", verify.ErrLoadTelemetryUnavailable,
			ErrUnavailable, reason)
	}
	return s.CPU(r.now())
}

// HostWithhold is why heavy autonomous maintenance should wait now
// (executor.HostGuardReader); none without fresh telemetry.
func (r *Runtime) HostWithhold(ctx context.Context) []string {
	if ctx.Err() != nil {
		return nil
	}
	s, history := r.current()
	if s == nil {
		return nil
	}
	now := r.now()
	return Withhold(*s, StorageRunway(history, now), now, r.limits)
}

// HostMemory is fresh total and available host memory (0 = unknown).
func (r *Runtime) HostMemory() (total, available int64) {
	s, _ := r.current()
	if s == nil {
		return 0, 0
	}
	return s.HostMemory(r.now())
}

// CapacityBytes is the bounded storage capacity
// (autonomy.DiskCapacityProvider).
func (r *Runtime) CapacityBytes(ctx context.Context) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s, _ := r.current()
	if s == nil {
		return 0, fmt.Errorf("%w: no fresh storage sample", ErrUnavailable)
	}
	_, capacity, ok := s.EffectiveFreeStorage(r.now())
	if !ok {
		return 0, fmt.Errorf("%w: storage capacity unknown or unbounded", ErrUnavailable)
	}
	return int64(capacity), nil
}

// SetDrift publishes the parameter drift the managed-change worker found.
func (r *Runtime) SetDrift(d []managedparam.Drift) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drift = append([]managedparam.Drift(nil), d...)
}
