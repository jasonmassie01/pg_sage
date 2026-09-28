package providerobs

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/logwatch"
)

type API interface {
	Metrics(context.Context) (string, error)
	Logs(context.Context, string, time.Time, time.Time) ([]logwatch.LogEntry, error)
}

type LogHandler func(context.Context, []logwatch.LogEntry) error

// Runtime belongs to one database and never discovers projects or changes provider configuration.
type Runtime struct {
	api       API
	database  string
	handler   LogHandler
	pollMu    sync.Mutex
	mu        sync.RWMutex
	previous  MetricSnapshot
	telemetry Telemetry
	through   time.Time
	seen      map[[32]byte]time.Time
}

func NewRuntime(api API, database string, handler LogHandler) (*Runtime, error) {
	if api == nil || database == "" {
		return nil, errors.New("observability requires API and database")
	}
	return &Runtime{api: api, database: database, handler: handler,
		seen: make(map[[32]byte]time.Time)}, nil
}

// Run polls once per minute and invalidates evidence when its owning database shuts down.
func (r *Runtime) Run(ctx context.Context, report func(error)) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	defer r.invalidate()
	for {
		if ctx.Err() != nil {
			return
		}
		if err := r.Poll(ctx, time.Now()); err != nil && report != nil {
			report(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Runtime) Poll(ctx context.Context, now time.Time) error {
	r.pollMu.Lock()
	defer r.pollMu.Unlock()
	if err := ctx.Err(); err != nil {
		r.invalidate()
		return err
	}
	metricErr := r.pollMetrics(ctx, now)
	logErr := r.pollLogs(ctx, now)
	return errors.Join(metricErr, logErr)
}

func (r *Runtime) pollMetrics(ctx context.Context, now time.Time) error {
	body, err := r.api.Metrics(ctx)
	if err != nil {
		r.invalidate()
		return err
	}
	current, err := ParseMetrics(body, now)
	if err != nil {
		r.invalidate()
		return err
	}
	sample := Telemetry{ObservedAt: current.ObservedAt, MemoryTotalBytes: current.MemoryTotalBytes,
		MemoryAvailableBytes: current.MemoryAvailableBytes}
	if !r.previous.ObservedAt.IsZero() {
		sample, err = DeriveTelemetry(r.previous, current, now)
	}
	r.previous = current
	r.mu.Lock()
	r.telemetry = sample
	r.mu.Unlock()
	return err
}

func (r *Runtime) invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.telemetry = Telemetry{}
}

func (r *Runtime) Telemetry() Telemetry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t := r.telemetry
	t.CPUPct = cloneValue(t.CPUPct)
	t.MemoryTotalBytes = cloneValue(t.MemoryTotalBytes)
	t.MemoryAvailableBytes = cloneValue(t.MemoryAvailableBytes)
	return t
}

func cloneValue(value *float64) *float64 {
	if value == nil {
		return nil
	}
	result := *value
	return &result
}

// CurrentCPU reports fresh host CPU utilization for load admission.
func (r *Runtime) CurrentCPU(ctx context.Context) (float64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return r.Telemetry().CPU(time.Now())
}
